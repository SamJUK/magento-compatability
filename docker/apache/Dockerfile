FROM php:${PHP_VERSION:-8.3}-fpm AS php_base

FROM httpd:2.4-alpine

LABEL maintainer="magento2-supported-versions"

# Enable required Apache modules
RUN sed -i \
        -e 's|#LoadModule deflate_module|LoadModule deflate_module|' \
        -e 's|#LoadModule expires_module|LoadModule expires_module|' \
        -e 's|#LoadModule headers_module|LoadModule headers_module|' \
        -e 's|#LoadModule proxy_module|LoadModule proxy_module|' \
        -e 's|#LoadModule proxy_fcgi_module|LoadModule proxy_fcgi_module|' \
        -e 's|#LoadModule rewrite_module|LoadModule rewrite_module|' \
        /usr/local/apache2/conf/httpd.conf

# Point DocumentRoot to Magento pub/
RUN sed -i \
        -e 's|DocumentRoot "/usr/local/apache2/htdocs"|DocumentRoot "/var/www/html/pub"|' \
        -e 's|<Directory "/usr/local/apache2/htdocs">|<Directory "/var/www/html/pub">|' \
        /usr/local/apache2/conf/httpd.conf

# Include our vhost config
RUN echo "Include conf/extra/magento.conf" >> /usr/local/apache2/conf/httpd.conf

COPY magento.conf /usr/local/apache2/conf/extra/magento.conf

EXPOSE 80

# Wait for the Magento pub/ directory to be created by the install step before
# starting Apache, because httpd validates DocumentRoot existence at startup.
CMD ["sh", "-c", "until [ -d /var/www/html/pub ]; do sleep 0.5; done && exec httpd-foreground"]
