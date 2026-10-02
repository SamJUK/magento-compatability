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
COPY --from=build /app/site/dist /usr/share/nginx/html
# Relative redirects (/check -> /check/) so they keep the scheme and host the
# TLS proxy in front was reached on.
RUN echo 'absolute_redirect off;' > /etc/nginx/conf.d/redirects.conf
# Serve the site's 404 page, which carries the RUM snippet like every other page.
RUN sed -i 's|#error_page  404              /404.html;|error_page  404              /404.html;|' /etc/nginx/conf.d/default.conf \
 && grep -q '^ *error_page  404 ' /etc/nginx/conf.d/default.conf
EXPOSE 80