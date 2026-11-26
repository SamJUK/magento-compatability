# Magento 2 / Mage-OS Compatibility Test Harness

Installs Magento 2 and Mage-OS releases against different PHP, database, search, cache, queue and Varnish versions, tests each install and records whether it worked.
The results are published at [magento.works](https://magento.works).

## Requirements

- Docker 24+ with Compose v2
- Go 1.22+
- Node.js 22.12+ and pnpm 12+ (`packageManager` in `site/package.json` pins the exact version)

No Magento auth keys are needed: Magento installs from the Hypernode mirror and Mage-OS from `mirror.mage-os.org`.

## Running tests

```bash
cd app && make build
./bin/magento-compatibility-builder test -product magento -version 2.4.8 -dry-run
```

`-dry-run` lists what would run. Drop it to run them.
Each run starts a stack, installs the release, runs smoke tests then the Playwright specs, and writes `results/{product}/{id}.json`.

Common runs:

```bash
# the recommended stack for every Magento release
./bin/magento-compatibility-builder test -baseline -product magento

# one release, with every service swapped one at a time
./bin/magento-compatibility-builder test -product magento -version 2.4.8

# two stacks at once (each takes 4 to 6 GB of RAM)
./bin/magento-compatibility-builder test -product magento -concurrency 2
```

Results already on disk are skipped unless you pass `-force`.
`test -h` lists every filter and option, including `-sample-data`, `-no-tui` for CI logs and `-playwright=false`.
Ctrl+C stops cleanly and writes no partial results.

## What gets tested

`matrix.yml` lists every release and the service versions to try.
Each release has a `baseline`, its recommended stack.
A run tests the baseline, then the baseline with one service swapped for each other version in `services`.

```yaml
- version: "2.4.8"
  baseline:
    php: "8.4"
    webserver: apache
    db: { type: mariadb, version: "11.4" }
    search: { type: opensearch, version: "2.19.5" }
    cache: { type: valkey, version: "8" }
    queue: { type: rabbitmq, version: "4.1" }
    varnish: "7.7"
```

To test a new version of a service, add it to `services`.
No code change is needed.

## Keeping the matrix up to date

The weekly `release-dates.yml` workflow runs `app/cmd/release-dates` and opens a PR with what changed.
To run it by hand: `cd app && go run ./cmd/release-dates`.

It does three things:

1. Adds every new stable release from each product's Composer repository.
   A new release copies the baseline of the version below it.
2. Aligns each Magento baseline with Adobe's [system-requirements.yml](https://github.com/AdobeDocs/commerce-operations.en/blob/main/_jekyll/_data/system-requirements.yml).
   If Adobe's exact version isn't in `services`, a later minor of the same major stands in (Varnish 7.7 for 7.5).
   If there is none, the run fails until you add that version to `services`.
3. Refreshes `site/src/data/release-dates.json` from [magento.watch](https://magento.watch/) by [Lukasz Bajsarowicz](https://github.com/lbajsarowicz) and the [Mage-OS GitHub releases](https://github.com/mage-os/mageos-magento2/releases).

Webserver and Mage-OS baselines are left alone, so check a new Mage-OS line by hand.
To stop the sync changing a baseline, list its version under `keep_baseline:`.
To keep a release out for good, list it under `skip:`.

A changed baseline changes what is tested, so run a sweep for every version in the PR before merging it.

`site/src/data/adobe-support.yml` is kept by hand from Adobe's [lifecycle policy](https://experienceleague.adobe.com/en/docs/commerce-operations/release/planning/lifecycle-policy).
Add a row when a new Magento line ships.

## Workarounds

Some releases only install with a core patch, a module swap or a Composer alias.
`docker/scripts/workarounds.json` lists each one with what it fixes, and every result records the ones it needed, so the site can flag a pass that depends on them.

To add one:

1. Put the `.patch` file in `docker/scripts/patches/`, or add the step to `apply_version_fixes()` in `docker/scripts/install.sh` with `echo "[WORKAROUND] <id>"`.
2. Add an entry with the same id to `workarounds.json`.

A patch is applied only to the versions its entry lists (`"2.4.4"` covers 2.4.4 and its `-pN` releases), only to the PHP versions in its optional `php` list, and only when `patch -p1 --dry-run` says it fits.

## Playwright

Three specs run on every install: the storefront renders, a guest checks out, and an admin saves a CMS block.
To run them against your own install:

```bash
cd docker/scripts/tests/playwright
npm install && npx playwright install --with-deps chromium
MAGENTO_BASE_URL=http://localhost:8080 npx playwright test
```

Your install needs the `e2e-checkout-product` that `docker/scripts/tests/seed-checkout-product.php` creates.
Set `PLAYWRIGHT_ADMIN_USER`, `PLAYWRIGHT_ADMIN_PASSWORD` and `PLAYWRIGHT_ADMIN_PATH` if its admin differs from `admin` / `Admin123!`.

## Service checks

After Playwright, each service gets a check that proves it works rather than just connected.
Each is its own step in the result, so results recorded before it existed stay valid, and a check exits 3 (recorded as a skip) when the release has nothing to test.
Run one by hand with `docker compose exec php-fpm bash /scripts/tests/run-check.sh php /scripts/tests/check-queue.php`.

- `queue`: schedules the bulk the admin mass attribute update schedules, runs `product_action_attribute.update` once, and checks the attribute changed and the bulk finished. Works over AMQP and STOMP.

## Results site

```bash
cd site
pnpm install
pnpm run dev
```

`pnpm run check` type-checks and `pnpm run build` builds the static site.
It reads `matrix.yml`, `results/` and `workarounds.json` at build time.

## Releasing and deploying

`./release.sh --publish` tags a release from the Conventional Commits since the last one.
The tag builds the site image (`ghcr.io/samjuk/magento-compatability`) and deploys it to the Swarm as the `magentoworks` stack (`deploy/stack.yml`).
To redeploy or roll back, run the Deploy workflow with an earlier image tag.

## Licence

OSL-3.0. Copyright (c) 2026 Sam James. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).

Releases up to v0.0.13 were published under MIT and stay under MIT.
