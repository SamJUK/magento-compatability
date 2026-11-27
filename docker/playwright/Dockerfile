# Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
# SPDX-License-Identifier: OSL-3.0
# Playwright for workers: the suite is mounted at /pw/suite and resolves
# its modules from /pw/node_modules. Build context: docker/scripts/tests/playwright.
ARG PLAYWRIGHT_VERSION=1.60.0
FROM mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-noble
WORKDIR /pw
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
