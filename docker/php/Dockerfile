ARG PHP_VERSION=8.3
FROM composer/composer:2.2.26-bin AS composer-lts
FROM composer/composer:2.9.8-bin AS composer-latest

FROM php:${PHP_VERSION}-fpm-alpine

WORKDIR /var/www/html

# Install system dependencies and additional tools (rarely changes)
RUN --mount=type=cache,target=/var/cache/apk \
    apk add --no-cache --update \
    bash \
    freetype-dev \
	libjpeg-turbo-dev \
	icu-dev \
	libxslt-dev \
	libzip-dev \
    jq yq git patch

# Install PHP extensions (changes with PHP version)
ARG PHP_EXTENSIONS="bcmath gd intl opcache pcntl pdo_mysql soap sockets xsl zip ftp"
RUN --mount=type=cache,target=/var/cache/apk \
    PHP_VER="$(echo ${PHP_VERSION} | tr -d '.')" \
    && PHP_EXTS= && for ext in $PHP_EXTENSIONS; do \
        # OPCache is bundled and enabled by default as of PHP 8.5+
        [[ "$ext" == "opcache" && "$PHP_VER" -ge "850" ]] && continue; \
        PHP_EXTS="${PHP_EXTS} ${ext}"; \
    done \
    && apk add --no-cache --update --virtual build-deps \
        g++ \
        shadow \
        curl-dev \
        oniguruma-dev \
        libpng-dev \
        libxml2-dev \
        zlib-dev \
        autoconf \
        linux-headers \
    # Setup PHP Extensions
    && docker-php-ext-configure gd --with-freetype --with-jpeg \
    && docker-php-ext-install -j$(nproc) ${PHP_EXTS} \
    # Cleanup - Remove build dependencies and temp files
	&& docker-php-source delete \
    && apk del build-deps \
    && rm -rf /tmp/* /var/cache/apk/*

COPY custom.ini /usr/local/etc/php/conf.d/

#
# Helper Tools
#
# Composer 2.2 LTS is the last line that supports PHP 7.4.
COPY --from=composer-lts /composer /tmp/composer-lts
COPY --from=composer-latest /composer /tmp/composer-latest
RUN if [[ "$PHP_VERSION" = 7.4* ]]; then \
        mv /tmp/composer-lts /usr/local/bin/composer; \
    else \
        mv /tmp/composer-latest /usr/local/bin/composer; \
    fi && \
    rm -f /tmp/composer-lts /tmp/composer-latest && \
    chmod 755 /usr/local/bin/composer && \
    composer --version

# Bake global Composer config into the image so concurrent containers don't
# race to write the same shared config.json on the mounted cache volume.
# Composer 2.2.x (used for PHP 7.4) does not support the newer audit config
# keys, so only apply those on the newer Composer line.
RUN composer config --global allow-plugins true && \
    if [[ "$PHP_VERSION" != 7.4* ]]; then \
        composer config --global audit.block-insecure false && \
        composer config --global audit.abandoned ignore; \
    fi

# Working directory — Magento is installed here by install.sh
WORKDIR /var/www/html

# Separate config home (image-internal, baked above) from download cache
# (shared named volume mounted at runtime — see compose/base.yml).
ENV COMPOSER_CACHE_DIR=/composer-cache

EXPOSE 9000

CMD ["php-fpm"]
