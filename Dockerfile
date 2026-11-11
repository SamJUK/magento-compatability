# Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
# SPDX-License-Identifier: OSL-3.0

FROM node:26-alpine AS build

WORKDIR /app

ENV PNPM_HOME="/pnpm"
ENV PATH="$PNPM_HOME:$PATH"
RUN npm install -g corepack && corepack enable

COPY site/package.json site/pnpm-lock.yaml site/pnpm-workspace.yaml ./site/
# Corepack takes the pnpm version from package.json in the working directory.
WORKDIR /app/site
RUN pnpm --dir /app/site/ install --frozen-lockfile

COPY site/ /app/site/
COPY ./matrix.yml /app/matrix.yml
COPY ./results/ /app/results/
COPY ./docker/scripts/workarounds.json /app/docker/scripts/workarounds.json

ARG MODE=production
ENV NODE_ENV=${MODE}
RUN pnpm --dir /app/site/ run build -- --mode ${MODE}

# Production Image
FROM nginx:alpine
# The base image lags Alpine's security fixes. The floors change the line, so a cached layer is rebuilt for them.
RUN apk upgrade --no-cache && apk add --no-cache 'curl>=8.23.0-r0' 'libcurl>=8.23.0-r0' 'brotli-libs>=1.2.1-r0' \
    'libxslt>=1.1.43-r4' 'libexpat>=2.10.0-r0' 'zlib>=1.4-r0'
COPY --from=build /app/site/dist /usr/share/nginx/html
COPY site/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80