package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

// composeFileMap maps service identifiers to their compose file basenames
// under compose/services/.
var composeFileMap = map[string]string{
	// databases
	"mariadb": "db-mariadb.yml",
	"mysql":   "db-mysql.yml",
	"percona": "db-percona.yml",
	// search engines
	"opensearch":    "search-opensearch.yml",
	"elasticsearch": "search-elasticsearch.yml",
	// cache
	"valkey": "cache-valkey.yml",
	"redis":  "cache-redis.yml",
	// queue
	"rabbitmq": "queue-rabbitmq.yml",
	"artemis":  "queue-artemis.yml",
}

// Compose is a thin wrapper around `docker compose` for a specific project
// stack. All methods accept a context so they respect cancellation and
// signal-driven timeouts.
type Compose struct {
	projectName string
	files       []string // -f flags, pre-expanded
	env         []string // KEY=VALUE pairs passed to the subprocess environment
}

// sanitiseProjectName derives a unique Docker Compose project name from a
// combination ID. A SHA-256 hash of the ID is used so that long combination
// names never collide after truncation — which caused "endpoint already exists"
// errors when running multiple combinations in parallel.
func sanitiseProjectName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("m2test-%x", sum[:8]) // 23 chars, well within the 63-char Docker limit
}

// parseHostPort extracts the port number from docker compose port output.
// Input format: "0.0.0.0:PORT\n" or just "PORT".
func parseHostPort(raw string) string {
	parts := strings.SplitN(strings.TrimSpace(raw), ":", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return strings.TrimSpace(raw)
}

// newCompose builds a Compose from a Combination, pre-resolving all compose
// file paths.  composeDir is the absolute path to the compose/ directory.
func newCompose(c matrix.Combination, composeDir string, extraEnv []string) (*Compose, error) {
	files := []string{filepath.Join(composeDir, "base.yml")}

	addService := func(typ string) error {
		name, ok := composeFileMap[typ]
		if !ok {
			return fmt.Errorf("compose: unknown service type %q", typ)
		}
		files = append(files, filepath.Join(composeDir, "services", name))
		return nil
	}

	if err := addService(c.DBType); err != nil {
		return nil, err
	}
	if err := addService(c.SearchType); err != nil {
		return nil, err
	}
	if err := addService(c.CacheType); err != nil {
		return nil, err
	}
	if err := addService(c.QueueType); err != nil {
		return nil, err
	}
	if c.Varnish != "none" && c.Varnish != "" {
		files = append(files, filepath.Join(composeDir, "services", "varnish.yml"))
	}

	projectName := sanitiseProjectName(c.ID())

	// Start from the current process environment so that PATH, HOME,
	// DOCKER_CONFIG, credential helpers, etc. are all inherited.
	env := append(os.Environ(),
		"COMPOSE_PROJECT_NAME="+projectName,
		"PHP_VERSION="+c.PHP,
		"WEBSERVER_TYPE="+c.WebserverType,
		"DB_VERSION="+c.DBVersion,
		"SEARCH_VERSION="+c.SearchVersion,
		"CACHE_VERSION="+c.CacheVersion,
		"QUEUE_VERSION="+c.QueueVersion,
		"VARNISH_VERSION="+c.Varnish,
		"WEBSERVER_PORT=0",
	)
	env = append(env, extraEnv...)

	return &Compose{
		projectName: projectName,
		files:       files,
		env:         env,
	}, nil
}

// args prepends the docker compose -f flags to subcommand args.
func (cp *Compose) args(sub ...string) []string {
	a := []string{"compose"}
	for _, f := range cp.files {
		a = append(a, "-f", f)
	}
	return append(a, sub...)
}

// run executes docker with the given arguments, capturing combined output.
// It inherits cp.env for the subprocess environment.
func (cp *Compose) run(ctx context.Context, sub ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", cp.args(sub...)...)
	cmd.Env = cp.env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// Up starts the stack with --wait (blocks until all healthchecks pass).
// Images are pre-built per PHP version; re-run 'docker compose build' manually
// after changing Dockerfiles.
func (cp *Compose) Up(ctx context.Context) (string, error) {
	return cp.run(ctx, "up", "-d", "--wait", "--wait-timeout", "120")
}

// removeEphemeralVolumes deletes the per-run volumes created by this stack.
// Compose names them {COMPOSE_PROJECT_NAME}_{suffix}. Errors are ignored —
// volumes may not exist if Up never succeeded. Uses a fresh context so cleanup
// runs even when the caller's context is already cancelled.
func (cp *Compose) removeEphemeralVolumes() {
	bg := context.Background()
	for _, suffix := range []string{"_magento", "_db-data", "_search-data"} {
		cmd := exec.CommandContext(bg, "docker", "volume", "rm", "--force", cp.projectName+suffix)
		cmd.Env = cp.env
		_ = cmd.Run()
	}
}

// Down tears down the stack. Per-run volumes (magento, db-data, search-data)
// are removed to prevent disk accumulation. The shared composer-cache and
// vendor-cache volumes are intentionally preserved so subsequent runs can
// reuse them.
func (cp *Compose) Down(ctx context.Context) error {
	_, err := cp.run(ctx, "down", "--remove-orphans")
	cp.removeEphemeralVolumes()
	return err
}

// Exec runs a command inside a service container (-T = no TTY allocation).
// args are appended after the service name.
func (cp *Compose) Exec(ctx context.Context, service string, args ...string) (string, error) {
	sub := append([]string{"exec", "-T", service}, args...)
	return cp.run(ctx, sub...)
}

// Port returns the host-side mapped port for containerPort on service.
func (cp *Compose) Port(ctx context.Context, service string, containerPort int) (string, error) {
	out, err := cp.run(ctx, "port", service, fmt.Sprintf("%d", containerPort))
	if err != nil {
		return "", err
	}
	return parseHostPort(out), nil
}

// Services lists the running service names for this project.
func (cp *Compose) Services(ctx context.Context) ([]string, error) {
	out, err := cp.run(ctx, "ps", "--services")
	if err != nil {
		return nil, err
	}
	var svcs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			svcs = append(svcs, line)
		}
	}
	return svcs, nil
}

// Logs captures the logs for a single service, capped at maxBytes.
// If maxBytes <= 0, all logs are returned.
func (cp *Compose) Logs(ctx context.Context, service string, maxBytes int64) (string, error) {
	out, _ := cp.run(ctx, "logs", "--no-color", "--timestamps", service)
	// Tail to maxBytes to keep result JSON files sane.
	if maxBytes > 0 && int64(len(out)) > maxBytes {
		out = out[int64(len(out))-maxBytes:]
		// Trim to the next newline so we don't split mid-line.
		if idx := strings.Index(out, "\n"); idx >= 0 {
			out = out[idx+1:]
		}
		out = "[... truncated to last " + byteCountSI(maxBytes) + " ...]\n" + out
	}
	return out, nil
}

// SetEnv appends additional KEY=VALUE pairs to the compose environment.
// Call before Up/Exec.
func (cp *Compose) SetEnv(pairs ...string) {
	cp.env = append(cp.env, pairs...)
}

// ProjectName returns the sanitised COMPOSE_PROJECT_NAME.
func (cp *Compose) ProjectName() string {
	return cp.projectName
}

// WriteLogs streams all service logs to w (used for debugging).
func (cp *Compose) WriteLogs(ctx context.Context, w io.Writer) error {
	out, _ := cp.run(ctx, "logs", "--no-color", "--timestamps")
	_, err := fmt.Fprint(w, out)
	return err
}

func byteCountSI(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}
