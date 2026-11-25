// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package pool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Arch is a worker's GOARCH.
func (p *Pool) Arch(ctx context.Context, name string) (string, error) {
	out, err := p.Script(ctx, name, "uname -m\n")
	if err != nil {
		return "", err
	}
	switch m := strings.TrimSpace(string(out)); m {
	case "x86_64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("%s: unsupported machine %q", name, m)
	}
}

func BuildWorker(ctx context.Context, appDir, goarch, out string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/worker")
	cmd.Dir = appDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goarch)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building m2test-worker: %w: %s", err, b)
	}
	return nil
}

// Setup installs a worker's binary, token and user unit; the unit starts at the first sweep, once a release is in.
func (p *Pool) Setup(ctx context.Context, name string, w *Worker, binary string) error {
	if err := p.Upload(ctx, name, binary, w.Dir+"/m2test-worker.new"); err != nil {
		return err
	}
	_, err := p.Script(ctx, name, setupScript(w))
	return err
}

func setupScript(w *Worker) string {
	d := w.Dir
	return fmt.Sprintf(`set -eu
umask 077
mkdir -p '%[1]s/bin' '%[1]s/releases' '%[1]s/incoming' '%[1]s/spool' "$HOME/.config/systemd/user"
chmod 755 '%[1]s/m2test-worker.new'
mv '%[1]s/m2test-worker.new' '%[1]s/bin/m2test-worker'
printf '%%s\n' '%[2]s' > '%[1]s/token.new'
mv '%[1]s/token.new' '%[1]s/token'
cat > "$HOME/.config/systemd/user/m2test-worker.service" <<'UNIT'
%[3]sUNIT
systemctl --user daemon-reload
systemctl --user enable m2test-worker
systemctl --user try-restart m2test-worker
`, d, w.Token, Unit(w))
}

// Unit is the worker's systemd user unit.
func Unit(w *Worker) string {
	d := w.Dir
	return fmt.Sprintf(`[Unit]
Description=m2test worker
# Enabled at setup; nothing to run until the first sweep deploys a release.
ConditionPathExists=%[1]s/releases/current

[Service]
ExecStart=%[1]s/bin/m2test-worker -socket %[1]s/worker.sock -token-file %[1]s/token -release %[1]s/releases/current -spool %[1]s/spool -slots %[2]d
Restart=on-failure
RestartSec=10
# Running stacks get a bounded teardown on stop.
TimeoutStopSec=5min

[Install]
WantedBy=default.target
`, d, w.Slots)
}

func (p *Pool) Deploy(ctx context.Context, name string, w *Worker, hash, tarball string) error {
	sum, err := fileSHA256(tarball)
	if err != nil {
		return err
	}
	have, err := p.Script(ctx, name, fmt.Sprintf("sha256sum '%s/releases/%s.tar.gz' 2>/dev/null | cut -d' ' -f1\n", w.Dir, hash))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(have)) != sum {
		if err := p.Upload(ctx, name, tarball, fmt.Sprintf("%s/incoming/%s.tar.gz", w.Dir, hash)); err != nil {
			return err
		}
	}
	if p.DockerAuth == "" {
		_, err = p.Script(ctx, name, deployScript(w.Dir, hash, ""))
		return err
	}
	// The token must never touch the worker's disk.
	_, err = p.script(ctx, name, deployScript(w.Dir, hash, p.DockerAuth), secretScript)
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 96:
			return fmt.Errorf("the Docker Hub token needs findmnt to check XDG_RUNTIME_DIR: %w", err)
		case 97:
			return fmt.Errorf("the Docker Hub token needs a tmpfs XDG_RUNTIME_DIR: %w", err)
		}
	}
	return err
}

func deployScript(d, hash, auth string) string {
	login := ""
	if auth != "" {
		login = fmt.Sprintf(`# A killed deploy skips its trap; the lock says none is running now.
rm -rf "${XDG_RUNTIME_DIR:?}"/m2test-docker.*
C=$(mktemp -d "${XDG_RUNTIME_DIR:?}/m2test-docker.XXXXXX")
trap 'rm -rf "$C"' EXIT
trap 'exit 1' HUP INT TERM
# Script input, never an argument.
cat > "$C/config.json" <<'AUTH'
{"auths":{"https://index.docker.io/v1/":{"auth":"%s"}}}
AUTH
export DOCKER_CONFIG="$C"
`, auth)
	}
	return fmt.Sprintf(`set -eu
# C is the token dir; one inherited from the shell must not be removed.
D='%s' H='%s' C=
# A deploy left running by an earlier sweep must not flip current back.
exec 9>"$D/deploy.lock"
timeout 600 flock 9
T="$D/incoming/$H.tar.gz"
test -s "$T" || T="$D/releases/$H.tar.gz"
%s"$D/bin/m2test-worker" prepare -hash "$H" -tarball "$T" -releases "$D/releases" >/dev/null
if [ -n "${C:-}" ]; then rm -rf "$C"; unset DOCKER_CONFIG; fi
# prepare checked it, so it becomes the copy the next sweep compares with.
if [ -s "$D/incoming/$H.tar.gz" ]; then mv "$D/incoming/$H.tar.gz" "$D/releases/$H.tar.gz"; fi
# What the worker last restarted on: after a failed restart, current is ahead of it,
# and a later deploy of that release must restart too.
if [ "$(cat "$D/running" 2>/dev/null || true)" = "$H" ] && [ "$(readlink "$D/releases/current" || true)" = "$H" ]; then
  systemctl --user start m2test-worker
else
  # Not this release's, whatever restarts next, until a restart says so.
  rm -f "$D/running"
  ln -sfn "$H" "$D/releases/current.new"
  mv -T "$D/releases/current.new" "$D/releases/current"
  systemctl --user restart m2test-worker
  echo "$H" > "$D/running"
fi
# Housekeeping, of our own images only: other releases' harness images and
# superseded builds. Images in use stay.
{ docker image prune -af --filter label=m2test.release --filter "label!=m2test.release=$H"
  docker image prune -f --filter label=m2test.release; } >/dev/null 2>&1 || true
`, d, hash, login)
}

// Stop disables a worker's unit, leaving its files.
func (p *Pool) Stop(ctx context.Context, name string) error {
	_, err := p.Script(ctx, name, "systemctl --user disable --now m2test-worker\n")
	return err
}

var Backoff = [2]time.Duration{time.Second, 30 * time.Second}

// Tunnel forwards the coordinator's worker.sock to a worker's Dir until
// ctx ends, reconnecting with backoff. sshd's StreamLocalBindUnlink lets a
// reconnect replace the socket a dropped session left.
func (p *Pool) Tunnel(ctx context.Context, name string, w *Worker, local string, lg *log.Logger) {
	wait := Backoff[0]
	for ctx.Err() == nil {
		started := time.Now()
		cmd := Command(ctx, p, "-N", "-R", w.Dir+"/worker.sock:"+local, name)
		out := &tail{max: 1 << 12}
		cmd.Stdout, cmd.Stderr = out, out
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			wait = Backoff[0]
		}
		lg.Printf("tunnel to %s down (%v: %s); reconnecting in %s", name, err, lastLines(out.Bytes(), 2), wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, Backoff[1])
	}
}

// DeployUntilDone retries Deploy with the tunnel's backoff: a worker that
// is still booting when the sweep starts gets the release once it's up.
func (p *Pool) DeployUntilDone(ctx context.Context, name string, w *Worker, hash, tarball string, lg *log.Logger) bool {
	wait := Backoff[0]
	for {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Minute)
		err := p.Deploy(attempt, name, w, hash, tarball)
		cancel()
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		wait = min(max(wait*2, 10*Backoff[0]), 10*Backoff[1])
		lg.Printf("%s: deploy failed, retrying in %s; it gets no work until then: %v", name, wait, err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}
}

func LocalSocket(path string) error {
	if strings.ContainsAny(path, ": %") || !filepath.IsAbs(path) {
		return fmt.Errorf("%s: ssh -R needs an absolute socket path with no colons, spaces or %%; set XDG_STATE_HOME", path)
	}
	return nil
}
