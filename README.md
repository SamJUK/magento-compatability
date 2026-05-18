# Magento 2 / MageOS Compatibility Test Harness

Installs every combination of Magento 2 / MageOS versions with supported service stacks,
runs smoke tests + Playwright E2E tests, and writes structured JSON results.
Results are rendered by an Astro static site.

> 🚧 This is currently in a early stage, Alpha release. Data may be incomplete and/or inaccurate. Use with caution. 🚧

---

## Requirements

| Tool | Min version | Notes |
|------|-------------|-------|
| Docker | 24+ | Compose v2 required |
| Go | 1.22+ | to build the CLI |
| Node.js | 20+ | Playwright + Astro site |

### macOS quick install
```bash
brew install go
# Docker Desktop provides Compose v2
```

---

## Building the CLI

```bash
cd app
make build
# output: app/bin/magento-compatibility-builder
```

---

## Running Tests

### Run all combinations for a product
```bash
./app/bin/magento-compatibility-builder test -product magento
```

### Run a single version (all service deviations)
```bash
./app/bin/magento-compatibility-builder test -product magento -version 2.4.8
```

### Run only the baseline for a specific version
```bash
./app/bin/magento-compatibility-builder test -baseline -product magento -version 2.4.8-p3
```

### Run all baselines across all versions
```bash
./app/bin/magento-compatibility-builder test -baseline -product magento
```

### Dry-run (print combinations without running)
```bash
./app/bin/magento-compatibility-builder test -product magento -version 2.4.8 -dry-run
```

### Run with concurrency
```bash
# Each stack uses ~4–6 GB RAM; only increase if you have headroom
./app/bin/magento-compatibility-builder test -product magento -concurrency 2
```

---

## CLI Flags — `test` subcommand

| Flag | Default | Description |
|------|---------|-------------|
| `-product` | _(all)_ | Filter by product (`magento`\|`mageos`) |
| `-version` | _(all)_ | Filter by product version (e.g. `2.4.8-p3`) |
| `-php` | _(all)_ | Filter by PHP version (e.g. `8.3`) |
| `-webserver` | _(all)_ | Filter by webserver (`nginx`\|`apache`) |
| `-db` | _(all)_ | Filter by database (`type:version`, e.g. `mariadb:11.4`) |
| `-search` | _(all)_ | Filter by search engine (e.g. `opensearch:3`) |
| `-cache` | _(all)_ | Filter by cache (e.g. `valkey:8`) |
| `-queue` | _(all)_ | Filter by queue (e.g. `rabbitmq:4.2`) |
| `-varnish` | _(all)_ | Filter by varnish version or `none` |
| `-baseline` | `false` | Run only the recommended baseline combination per version |
| `-concurrency` | `1` | Parallel stacks |
| `-force` | `false` | Re-run combinations that already have a result on disk |
| `-dry-run` | `false` | Print matching combinations without running |
| `-list-json` | `false` | Print combinations as JSON and exit |
| `-playwright` | `true` | Run Playwright E2E tests after smoke |
| `-no-tui` | `false` | Disable TUI; plain log output suitable for CI |
| `-max-log-bytes` | `1048576` | Max bytes captured per container log (`0` = unlimited) |
| `-matrix` | _(auto)_ | Path to matrix.yml |
| `-results-dir` | _(auto)_ | Path to results directory |
| `-compose-dir` | _(auto)_ | Path to compose directory |

---

## matrix.yml

Single source of truth for products, versions, and service dimensions.
The `baseline` for each version defines the recommended service set.
All other service entries generate deviation combinations.

```yaml
products:
  - name: magento
    package: magento/project-community-edition
    mirror: https://mage-os.hypernode.com/mirror/
    versions:
      - version: "2.4.8"
        baseline:
          php: "8.4"
          webserver: nginx
          db: { type: mariadb, version: "11.4" }
          search: { type: opensearch, version: "3" }
          cache: { type: valkey, version: "8" }
          queue: { type: rabbitmq, version: "4.2" }
          varnish: "none"

services:
  php: ["8.2", "8.3", "8.4"]
  database:
    - { type: mariadb, version: "11.4" }
    - { type: mysql, version: "8.0" }
  ...
```

Edit `matrix.yml` to add versions or services. No code changes required.

---

## Result Files

Each combination writes `results/{product}/{combo-id}.json`:

```json
{
  "id": "magento-248-php84-mariadb114-opensearch3-valkey8-rabbitmq42-nginx",
  "product": "magento",
  "version": "2.4.8",
  "overall_status": "pass",
  "services": {
    "php": "8.4",
    "webserver": "nginx",
    "db":     { "type": "mariadb",    "version": "11.4" },
    "search": { "type": "opensearch", "version": "3"    },
    "cache":  { "type": "valkey",     "version": "8"    },
    "queue":  { "type": "rabbitmq",   "version": "4.2"  },
    "varnish": "none"
  },
  "steps": {
    "stack_up":   { "status": "pass", "duration_s": 45,  "log": "..." },
    "install":    { "status": "pass", "duration_s": 312, "log": "..." },
    "smoke":      { "status": "pass", "duration_s": 180, "log": "..." },
    "playwright": { "status": "skip", "duration_s": 0,   "log": "..." }
  },
  "container_logs": { "php-fpm": "...", "db": "..." },
  "timestamp": "2025-01-15T03:00:00Z"
}
```

---

## Patching

Version-specific core bugs are fixed automatically during install.
Patch files live in `docker/scripts/patches/` and are applied on every install
(regardless of vendor cache state) using `patch -p1 --dry-run` to check applicability
before applying. Inapplicable patches are silently skipped.

To add a patch:
1. Drop a `.patch` file into `docker/scripts/patches/`
2. Add an entry to `docker/scripts/patches.json`
3. The patch will be applied (or silently skipped) on the next run

Version-specific Composer constraint fixes (e.g. 2.4.4 package aliasing) are
handled in `docker/scripts/install.sh` → `apply_version_fixes()`.

---

## Playwright Tests

Playwright tests run inside the host Node environment after smoke tests pass.
To run standalone against a live Magento instance:

```bash
cd docker/scripts/tests/playwright
npm install
npx playwright install --with-deps chromium
MAGENTO_BASE_URL=http://localhost:8080 npx playwright test
```

---

## Results Site

```bash
cd site
npm install
npm run dev      # development server
npm run build    # production static build
```

Pages:
- `/magento` / `/mageos` — version overview matrix
- `/magento/{version}` — full service compatibility breakdown
- `/magento/baseline` — baseline-only results for all versions

---

## Notes

- **No Magento auth keys required** — packages served from Hypernode mirror; MageOS uses `https://mirror.mage-os.org/`
- **Vendor cache** — Docker volume `m2test-vendor-cache` persists downloaded packages keyed by `{php}-{package}-{version}`; patches are applied on every install after `composer install`
- **2FA** — disabled automatically during install so Playwright can log in
- **Sample data** — not installed by default; set `INSTALL_SAMPLE_DATA=1` to enable
- **Varnish** — when enabled, webserver listens on 8080 internally; Varnish fronts on port 80
- **CTRL+C** — cancels cleanly; partial results are not written to disk
