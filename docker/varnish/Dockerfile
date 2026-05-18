FROM varnish:7.7

LABEL maintainer="magento2-supported-versions"

# Copy Magento-specific VCL
COPY default.vcl /etc/varnish/default.vcl

# Listen on port 80; use port 8080 as the backend (the web server container)
ENV VARNISH_HTTP_PORT=80
ENV VARNISH_MEMORY=256m

EXPOSE 80

# Use shell form so ${VARNISH_MEMORY} is expanded at runtime
CMD varnishd -F \
    -f /etc/varnish/default.vcl \
    -s "malloc,${VARNISH_MEMORY}" \
    -a "0.0.0.0:${VARNISH_HTTP_PORT}" \
    -T "0.0.0.0:6082"
