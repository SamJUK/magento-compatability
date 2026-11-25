// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var maxManifest = 4 << 20

// scheme is http only in tests.
var scheme = "https"

const accept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

type Ref struct {
	Registry, Repo, Tag string
}

func ParseRef(s string) (Ref, error) {
	if strings.Contains(s, "@") {
		return Ref{}, fmt.Errorf("%s is already pinned", s)
	}
	r := Ref{Registry: "registry-1.docker.io", Tag: "latest"}
	name := s
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, r.Tag = name[:i], name[i+1:]
	}
	first, rest, ok := strings.Cut(name, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, name = first, rest
		if first == "docker.io" || first == "index.docker.io" {
			r.Registry = "registry-1.docker.io"
		}
	}
	if r.Registry == "registry-1.docker.io" && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	if name == "" || r.Tag == "" {
		return Ref{}, fmt.Errorf("bad image reference %q", s)
	}
	r.Repo = name
	return r, nil
}

// Resolve asks each image's registry for the digest its tag points at now.
// A multi-arch tag resolves to its index, so arm64 and amd64 hosts agree.
func Resolve(ctx context.Context, refs []string) (map[string]string, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	out := map[string]string{}
	for _, s := range refs {
		r, err := ParseRef(s)
		if err != nil {
			return nil, err
		}
		d, err := digest(ctx, c, r)
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", s, err)
		}
		out[s] = d
	}
	return out, nil
}

// digest asks with HEAD, which Docker Hub doesn't count as a pull, and
// falls back to GET and hashing the body when no digest header comes back.
func digest(ctx context.Context, c *http.Client, r Ref) (string, error) {
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, r.Registry, r.Repo, url.PathEscape(r.Tag))
	token := ""
	method := http.MethodHead
	for try := 0; try < 4; try++ {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", accept)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.Do(req)
		if err != nil {
			return "", err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxManifest)+1))
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		if len(body) > maxManifest {
			// Hashing a prefix would pin a digest that doesn't exist.
			return "", fmt.Errorf("%s: manifest over %d bytes", u, maxManifest)
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && token == "":
			if token, err = anonToken(ctx, c, resp.Header.Get("WWW-Authenticate")); err != nil {
				return "", err
			}
			continue
		case resp.StatusCode != http.StatusOK:
			return "", &statusError{u, resp.StatusCode, resp.Status}
		case method == http.MethodHead:
			if d := resp.Header.Get("Docker-Content-Digest"); validDigest(d) {
				return d, nil
			}
			method = http.MethodGet
			continue
		}
		sum := sha256.Sum256(body)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	return "", fmt.Errorf("%s: no digest", u)
}

type statusError struct {
	url    string
	code   int
	status string
}

func (e *statusError) Error() string { return e.url + ": " + e.status }

// unpublished asks the registry, rather than reading a failed pull's
// error: no such tag, or a package it won't show anonymously.
var unpublished = func(ctx context.Context, ref string) bool {
	r, err := ParseRef(ref)
	if err != nil {
		return false
	}
	_, err = digest(ctx, &http.Client{Timeout: 30 * time.Second}, r)
	var se *statusError
	return errors.As(err, &se) && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden || se.code == http.StatusNotFound)
}

func validDigest(d string) bool {
	h, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

func anonToken(ctx context.Context, c *http.Client, challenge string) (string, error) {
	kind, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(kind, "Bearer") {
		return "", fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	p := map[string]string{}
	for _, kv := range strings.Split(params, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		p[k] = strings.Trim(v, `"`)
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || p["realm"] == "" || (realm.Scheme != "https" && realm.Scheme != scheme) {
		return "", fmt.Errorf("bad token realm %q", p["realm"])
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &statusError{"token", resp.StatusCode, resp.Status}
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", err
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return t.Token, nil
}
