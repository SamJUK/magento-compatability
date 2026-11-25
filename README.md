# Magento 2 / Mage-OS Compatibility Test Harness

Installs Magento 2 and Mage-OS releases against different PHP, database, search, cache, queue and Varnish versions, tests each install and records whether it worked.
The results are published at [magento.works](https://magento.works).

## Requirements

- Linux or macOS (Windows isn't supported)
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
`-deadline 45m` fails a combination that runs longer, with `deadline_exceeded`, and still writes its result.

### Long sweeps

`queue` works through a sweep unattended and picks up where it left off after Ctrl+C, a crash or a reboot:

```bash
./bin/magento-compatibility-builder queue -product magento -version 2.4.8
./bin/magento-compatibility-builder queue -ids-file sweep.txt   # one combination ID per line
./bin/magento-compatibility-builder queue -ids-file sweep.txt -status
```

It takes the same filters and options as `test`, with `-deadline` at 45 minutes by default.
Stack setup failures are retried up to three times, and any other failure from the sweep gets one more go.
Failures from before the sweep run once with `-retry-failures`, and passes once with `-force`.
A sweep is the same filters and results directory; it remembers when it started, so a failure written since then (by `test` too) counts as the sweep's own until `-fresh` starts it over.
Resuming a sweep with different `-playwright`, `-sample-data`, `-max-log-bytes` or `-deadline` is refused, since its results would be mixed; `-fresh` starts it over.
A run that errors without writing a result is put down to the host and retried up to three times without using up the combination's tries.
It runs 2 stacks at once (`-slots`), drops one while memory is short and waits while the disk is under 40GB free.
Writing a number to the `slots` file it names at startup changes the slot count without a restart, up to `-slots` or the file's value at startup, whichever is larger.
A second Ctrl+C stops at once without tearing down running stacks; the next `queue` start removes them.
One queue runs per machine. Its state lives in `~/.local/state/magento-compatibility/queue/ledger.json`.
`test` skips any combination a queue is running at that moment, and the queue skips any `test` is running; `test -ignore-queue` runs it anyway.

### Remote workers

Workers live in an inventory in `~/.config/magento-compatibility/` (`$XDG_CONFIG_HOME`), next to a pool SSH key, ssh config and known hosts the tool writes.
Every connection to a worker uses that config: the pool key only, no agent, and the host key pinned to the worker's name.

```bash
# On the worker host: its key fingerprint, read there rather than trusted on first use
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
# On this machine: pin it, make a token, and print the pool key to authorise for m2test there
./bin/magento-compatibility-builder worker add lab1 -host 192.0.2.10 -host-key SHA256:...
# Install m2test-worker (built here for the host's arch), its token and its systemd user unit
./bin/magento-compatibility-builder worker setup lab1
./bin/magento-compatibility-builder queue -ids-file sweep.txt -workers lab1 -slots 0
```

The host needs Docker with buildx and Compose, an `m2test` user in the docker group whose only key is the pool key, lingering for that user, `/var/lib/m2test` owned by it, and these sshd settings for it: `StreamLocalBindUnlink yes` (a reconnect replaces a stale socket) and `AllowTcpForwarding remote` with `PermitListen none` (OpenSSH refuses a forwarded Unix socket under `AllowTcpForwarding no`).

For each remote worker, `queue -workers` keeps an SSH tunnel open from the worker's `/var/lib/m2test/worker.sock` to `worker.sock` here, reconnecting with backoff, and deploys the sweep's release to it before restarting its unit.
A remote worker serves one sweep at a time: a second sweep on this machine naming it stops at once.
`worker add NAME` with no `-host` makes a worker for this machine, which runs `m2test-worker -socket <state dir>/worker.sock` itself.
`worker remove` stops a worker's unit and forgets it and its host key; `worker list` shows them.
`-slots 0` leaves every run to the workers.

A sweep with `-workers` runs a release: a snapshot of `matrix.yml` and `docker/` with every upstream image pinned to the digest its tag had at the start, in `releases/` in the state directory.
Resuming the sweep runs the same release, even if the checkout or the upstream tags have moved since; `-fresh` takes a new one.
Local slots run the release too, on its pinned images and with Playwright in its container, so `-compose-dir` can't be used with `-workers`.
Readying those images retags this machine's upstream tags (`mariadb:11.4` and so on) to the pinned digests.

Each worker unpacks the release tarball, refusing it unless it holds the release the queue asked for, and readies its images: it pulls each pinned image by digest and builds the harness images (`m2test/*`) on them.
The queue does this through `m2test-worker prepare -hash <hash> -tarball <file> -releases <dir>`, which works by hand too.

The queue only hands work to a worker on its release, and a worker missing any of the release's images, or holding harness images built for another release, takes none until `prepare` has run.
Workers run Playwright in a container on the stack's own network, against `http://varnish` or `http://webserver`, so they need no Node.
A sweep started without `-workers` runs the checkout, so it can't take workers later without `-fresh`.

A worker keeps each finished run in its spool until the queue has answered for it, so results outlive a dropped tunnel or a restarted queue.
A worker that goes quiet for two minutes loses its runs to the next claim; its late uploads are refused.
The queue checks every uploaded result against the combination it handed out and keeps any it refuses in `rejected/`.
`test -ignore-queue` aborts a worker's run of the same combination and refuses its upload.

### Cloud bursts

`queue -cloud hetzner:N` rents up to N Hetzner servers for the sweep, sets each up as a worker as it comes up, and destroys them once they are no longer needed: an idle one as soon as nothing is waiting (keeping one until the sweep ends, for late retries), any past `max_age` once its runs finish, and all of them when the sweep stops, Ctrl+C included.
It needs `HCLOUD_TOKEN` and `pool.yml` beside the inventory:

```yaml
ssh_from: [198.51.100.4/32]   # required: who may reach workers over SSH
hetzner: {type: ccx33, location: hel1, image: debian-13, slots: 5}
max_workers: 10
max_burst_cost: 10            # euros; a bigger estimate needs -yes
max_age: 8h
minutes_per_run: 4            # for the estimate
setup_minutes: 10
```

Everything but `ssh_from` has the default shown.
Before creating anything the queue prints the estimate: each worker billed in whole hours, at Hetzner's gross price for the server and its IPv4 address, as if every one ran to the end.
Each server gets a host key made on this machine and pinned before it exists, and cloud-init installs Docker, the `m2test` user with only the pool key, and the sshd settings above, and drops bridged container traffic to the metadata service (a host-network container still reaches it).
Each pool gets a random ID (in `pool-id` beside `pool.yml`), so two machines sharing a project never touch each other's servers.
The pool key also goes on root (as the `m2test-pool-<id>` key in the project) so Hetzner doesn't email root passwords; cloud-init then disables root login.
A firewall named `m2test-<id>` allows SSH from `ssh_from` only.

If the queue dies without destroying its servers (a second Ctrl+C, a crash), they stay in the inventory and the next burst refuses to start.
`builder worker gc` lists them and every server labelled `m2test=worker,m2test-pool=<id>` that the inventory doesn't know; `-yes` destroys them.

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
