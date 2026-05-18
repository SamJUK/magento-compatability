FROM nginx:1.28-alpine

LABEL maintainer="magento2-supported-versions"

# Remove default site
RUN rm /etc/nginx/conf.d/default.conf

# Copy Magento-optimised Nginx config
COPY magento.conf /etc/nginx/conf.d/magento.conf

EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
