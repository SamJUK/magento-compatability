vcl 4.1;

# Backend is the web server (nginx or apache) — always listens on port 80
backend default {
    .host = "webserver";
    .port = "80";
    .connect_timeout = 60s;
    .first_byte_timeout = 60s;
    .between_bytes_timeout = 60s;
}

# Only allow purge from localhost / internal containers
acl purge {
    "localhost";
    "127.0.0.1";
    "10.0.0.0/8";
    "172.16.0.0/12";
    "192.168.0.0/16";
}

sub vcl_recv {
    # Handle PURGE requests (Magento cache invalidation)
    if (req.method == "PURGE") {
        if (!client.ip ~ purge) {
            return (synth(405, "Method not allowed"));
        }
        return (purge);
    }

    # Ban regex purge (used by Magento's Varnish module)
    if (req.method == "BAN") {
        if (!client.ip ~ purge) {
            return (synth(405, "Method not allowed"));
        }
        ban("obj.http.X-Magento-Tags ~ " + req.http.X-Magento-Tags);
        return (synth(200, "Banned"));
    }

    # Only cache GET and HEAD
    if (req.method != "GET" && req.method != "HEAD") {
        return (pass);
    }

    # Don't cache requests with auth headers
    if (req.http.Authorization) {
        return (pass);
    }

    # Don't cache admin
    if (req.url ~ "^/admin" || req.url ~ "^/index.php/admin") {
        return (pass);
    }

    # Strip cookies except session and visitor tracking (Magento sets these)
    set req.http.Cookie = regsuball(req.http.Cookie, ";?\s*(XDEBUG_[A-Z_]+|hasFilterParam)=[^;]+", "");
    set req.http.Cookie = regsuball(req.http.Cookie, "^\s*;\s*", "");

    if (req.http.Cookie == "") {
        unset req.http.Cookie;
    }

    return (hash);
}

sub vcl_hash {
    hash_data(req.url);
    if (req.http.host) {
        hash_data(req.http.host);
    } else {
        hash_data(server.ip);
    }

    # Vary cache by X-Magento-Vary cookie (store view, customer group, etc.)
    if (req.http.cookie ~ "X-Magento-Vary=") {
        hash_data(regsub(req.http.cookie, ".*X-Magento-Vary=([^;]+);?.*", "\1"));
    }

    if (req.http.HTTPS) {
        hash_data("https");
    }

    return (lookup);
}

sub vcl_backend_response {
    # Cache 404s for a short time
    if (beresp.status == 404) {
        set beresp.ttl = 60s;
    }

    # Don't cache if the backend says not to
    if (beresp.http.Cache-Control ~ "no-cache" || beresp.http.Cache-Control ~ "no-store" || beresp.http.Pragma ~ "no-cache") {
        set beresp.uncacheable = true;
        set beresp.ttl = 120s;
        return (deliver);
    }

    # Grace period for stale content while fetching fresh
    set beresp.grace = 1m;

    return (deliver);
}

sub vcl_deliver {
    # Add cache HIT/MISS debug header
    if (obj.hits > 0) {
        set resp.http.X-Cache = "HIT";
        set resp.http.X-Cache-Hits = obj.hits;
    } else {
        set resp.http.X-Cache = "MISS";
    }

    # Strip Varnish internals from response
    unset resp.http.X-Magento-Tags;
    unset resp.http.X-Powered-By;
    unset resp.http.Server;

    return (deliver);
}
