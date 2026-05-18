FROM node:23-alpine AS build

WORKDIR /app

ENV PNPM_HOME="/pnpm"
ENV PATH="$PNPM_HOME:$PATH"
RUN corepack enable

COPY site/package.json site/pnpm-lock.yaml site/pnpm-workspace.yaml ./site/
RUN pnpm --dir /app/site/ install --frozen-lockfile

COPY site/ /app/site/
COPY ./matrix.yml /app/matrix.yml
COPY ./results/ /app/results/

ARG MODE=production
ENV NODE_ENV=${MODE}
RUN pnpm --dir /app/site/ run build -- --mode ${MODE}

# Production Image
FROM nginx:alpine
COPY --from=build /app/site/dist /usr/share/nginx/html
EXPOSE 80