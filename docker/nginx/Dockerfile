# Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
# SPDX-License-Identifier: OSL-3.0

ARG WEBSERVER_VERSION=1.30
FROM nginx:${WEBSERVER_VERSION}-alpine

LABEL maintainer="magento2-supported-versions"

# Remove default site
RUN rm /etc/nginx/conf.d/default.conf

# Copy Magento-optimised Nginx config
COPY magento.conf /etc/nginx/conf.d/magento.conf

EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
