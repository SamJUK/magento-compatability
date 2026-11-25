// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package pool keeps the worker inventory and the tool-owned SSH setup
// every connection to a worker goes through.
package pool

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const DefaultDir = "/var/lib/m2test"

// Worker is one inventory entry. One with no Host runs on this machine
// and connects to worker.sock directly.
type Worker struct {
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	User    string `json:"user,omitempty"`
	Dir     string `json:"dir,omitempty"`
	HostKey string `json:"host_key,omitempty"` // "<type> <base64>", pinned at add
	Token   string `json:"token"`
	Slots   int    `json:"slots,omitempty"`
	Cloud   *Cloud `json:"cloud,omitempty"`
}

// Cloud marks a worker a burst made, to be destroyed with it. ID is 0 until
// the provider has answered; the nonce finds the server if it never does.
type Cloud struct {
	Provider string    `json:"provider"`
	Region   string    `json:"region,omitempty"`  // AWS only
	Account  string    `json:"account,omitempty"` // AWS only
	ID       ServerID  `json:"id,omitempty"`
	Nonce    string    `json:"nonce"`
	Created  time.Time `json:"created"`
}

type ServerID string

// UnmarshalJSON also takes the numbers inventories held before AWS.
func (id *ServerID) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*id = ServerID(n)
		return nil
	}
	return json.Unmarshal(b, (*string)(id))
}

func (w *Worker) Remote() bool { return w.Host != "" || w.Cloud != nil }

type Inventory struct {
	Workers map[string]*Worker `json:"workers"`
}

type Pool struct {
	Dir        string
	DockerAuth string
}

var dockerUser = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// DockerHubAuth checks a Docker Hub user and read-only token, both or
// neither, and encodes them for a Docker config.
func DockerHubAuth(user, token string) (string, error) {
	switch {
	case user == "" && token == "":
		return "", nil
	case !dockerUser.MatchString(user) || token == "" || strings.ContainsAny(token, " \t\r\n"):
		return "", fmt.Errorf("DOCKERHUB_USERNAME and DOCKERHUB_TOKEN go together, as a Docker Hub user and token")
	}
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + token)), nil
}

func Open() (*Pool, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".config")
	}
	p := &Pool{Dir: filepath.Join(base, "magento-compatibility")}
	// ssh expands % in IdentityFile and UserKnownHostsFile.
	if strings.Contains(p.Dir, "%") {
		return nil, fmt.Errorf("%s: no %% in XDG_CONFIG_HOME, please", p.Dir)
	}
	return p, os.MkdirAll(filepath.Join(p.Dir, "ssh"), 0o700)
}

// MaxSlots catches typos; a host running more parallel Magento stacks than this is unlikely.
const MaxSlots = 64

func (p *Pool) Lock(name string) (*os.File, error) {
	f, err := os.OpenFile(p.path(name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("worker %s is in use by another sweep", name)
	}
	return f, nil
}

func (p *Pool) path(name string) string { return filepath.Join(p.Dir, name) }
func (p *Pool) sshPath(name string) string {
	return filepath.Join(p.Dir, "ssh", name)
}

var (
	validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	validHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]*$`)
	validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	// Goes into remote shell scripts and the ssh -R spec unquoted.
	validDir   = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	validKey   = regexp.MustCompile(`^[a-z0-9@.-]+ [A-Za-z0-9+/]+=*$`)
	validToken = regexp.MustCompile(`^[A-Za-z0-9]{32,}$`)
	validNonce = regexp.MustCompile(`^[a-f0-9]{8,32}$`)
	// Hetzner's numbers or EC2's i-0123abcd; "" while unknown.
	validServerID = regexp.MustCompile(`^([1-9][0-9]{0,18}|i-[0-9a-f]{8,17})?$`)
	validRegion   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	validAccount  = regexp.MustCompile(`^[0-9]{12}$`)
)

func (w *Worker) validate(name string) error {
	switch {
	case !validName.MatchString(name):
		return fmt.Errorf("worker name %q: use a-z, 0-9 and -", name)
	case !validToken.MatchString(w.Token):
		return fmt.Errorf("worker %s has no token", name)
	case !w.Remote():
		return nil
	case w.Cloud != nil && (!(w.Cloud.Provider == "hetzner" && w.Cloud.Region == "" && w.Cloud.Account == "" ||
		w.Cloud.Provider == "aws" && ValidRegion(w.Cloud.Region) && (w.Cloud.Account == "" || validAccount.MatchString(w.Cloud.Account))) || !validNonce.MatchString(w.Cloud.Nonce) || !validServerID.MatchString(string(w.Cloud.ID))):
		return fmt.Errorf("worker %s: bad cloud entry", name)
	case w.Host == "" && w.Cloud.ID != "":
		return fmt.Errorf("worker %s: cloud server %s has no address", name, w.Cloud.ID)
	case len(w.Dir+"/worker.sock") > 107: // Linux's sun_path, less its NUL
		return fmt.Errorf("worker %s: dir %s is too long for a socket path", name, w.Dir)
	case w.Slots < 1 || w.Slots > MaxSlots:
		return fmt.Errorf("worker %s: slots must be 1 to %d, not %d", name, MaxSlots, w.Slots)
	case w.Host != "" && !validHost.MatchString(w.Host):
		return fmt.Errorf("worker %s: bad host %q", name, w.Host)
	case !validUser.MatchString(w.User):
		return fmt.Errorf("worker %s: bad user %q", name, w.User)
	case !validDir.MatchString(w.Dir) || path.Clean(w.Dir) != w.Dir:
		return fmt.Errorf("worker %s: bad dir %q", name, w.Dir)
	case !validKey.MatchString(w.HostKey):
		return fmt.Errorf("worker %s has no pinned host key", name)
	case w.Port < 1 || w.Port > 65535:
		return fmt.Errorf("worker %s: bad port %d", name, w.Port)
	}
	return nil
}

func (p *Pool) Load() (*Inventory, error) {
	inv := &Inventory{Workers: map[string]*Worker{}}
	data, err := os.ReadFile(p.path("workers.json"))
	if errors.Is(err, os.ErrNotExist) {
		return inv, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, inv); err != nil {
		return nil, fmt.Errorf("%s: %w", p.path("workers.json"), err)
	}
	if inv.Workers == nil {
		inv.Workers = map[string]*Worker{}
	}
	return inv, inv.validate()
}

// ValidRegion says if s looks like an AWS region name.
func ValidRegion(s string) bool { return validRegion.MatchString(s) }

// validate also refuses two entries for one token or one worker dir: they
// would shadow each other on worker.sock, or race on the same host.
func (inv *Inventory) validate() error {
	tokens, dirs := map[string]string{}, map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(inv.Workers)) {
		w := inv.Workers[name]
		if w == nil {
			return fmt.Errorf("worker %s is empty", name)
		}
		if err := w.validate(name); err != nil {
			return err
		}
		if other, ok := tokens[w.Token]; ok {
			return fmt.Errorf("workers %s and %s share a token", other, name)
		}
		tokens[w.Token] = name
		if w.Host == "" {
			continue
		}
		// One worker per user on a host: the unit is per user. The pinned
		// key names the host however it's addressed.
		for _, at := range []string{fmt.Sprintf("%s@%s:%d", w.User, w.Host, w.Port), w.User + " " + w.HostKey} {
			if other, ok := dirs[at]; ok {
				return fmt.Errorf("workers %s and %s are the same worker", other, name)
			}
			dirs[at] = name
		}
	}
	return nil
}

// Update changes the inventory under a lock, so two commands, or a burst's
// parallel creates, cannot lose each other's changes.
func (p *Pool) Update(change func(*Inventory) error) error {
	f, err := os.OpenFile(p.path("workers.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	inv, err := p.Load()
	if err != nil {
		return err
	}
	if err := change(inv); err != nil {
		return err
	}
	return p.Save(inv)
}

// ID names this pool's cloud resources, so two machines sharing an
// account never touch each other's.
func (p *Pool) ID() (string, error) {
	data, err := os.ReadFile(p.path("pool-id"))
	if err == nil {
		// Never remade: servers labelled with the old one would be lost.
		if id := strings.TrimSpace(string(data)); validNonce.MatchString(id) {
			return id, nil
		}
		return "", fmt.Errorf("%s is not a pool ID; its cloud servers are labelled m2test-pool=<it>", p.path("pool-id"))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	return id, writeAtomic(p.path("pool-id"), []byte(id+"\n"))
}

// HostKeyPair makes an ed25519 host key for a server not yet created, so
// its key is pinned before anything answers at its address.
func HostKeyPair(comment string) (private, public string, err error) {
	dir, err := os.MkdirTemp("", "m2test-hostkey")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	key := filepath.Join(dir, "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", key).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("ssh-keygen: %v: %s", err, out)
	}
	priv, err := os.ReadFile(key)
	if err != nil {
		return "", "", err
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", "", err
	}
	return string(priv), strings.TrimSpace(string(pub)), nil
}

// Save writes the inventory, then the ssh config and known hosts from it,
// so a removed worker's pinned key goes with it.
func (p *Pool) Save(inv *Inventory) error {
	if err := inv.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	var cfg, known bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(inv.Workers)) {
		w := inv.Workers[name]
		if w.Host == "" {
			continue
		}
		fmt.Fprintf(&cfg, "Host %s\n  HostName %s\n  Port %d\n  User %s\n  HostKeyAlias %s\n\n", name, w.Host, w.Port, w.User, name)
		fmt.Fprintf(&known, "%s %s\n", name, w.HostKey)
	}
	// ssh takes the first value it finds, so the defaults come last.
	fmt.Fprintf(&cfg, `Host *
  IdentityFile %q
  IdentitiesOnly yes
  IdentityAgent none
  ForwardAgent no
  ForwardX11 no
  BatchMode yes
  ServerAliveInterval 15
  ServerAliveCountMax 3
  ConnectTimeout 15
  ExitOnForwardFailure yes
  StrictHostKeyChecking yes
  UserKnownHostsFile %q
  GlobalKnownHostsFile /dev/null
  CheckHostIP no
  VerifyHostKeyDNS no
  UpdateHostKeys no
  ControlMaster no
  ControlPath none
  LogLevel ERROR
`, p.sshPath("id_ed25519"), p.sshPath("known_hosts"))
	if err := writeAtomic(p.sshPath("known_hosts"), known.Bytes()); err != nil {
		return err
	}
	if err := writeAtomic(p.sshPath("config"), cfg.Bytes()); err != nil {
		return err
	}
	// Last, so a failed write above leaves the inventory as it was.
	return writeAtomic(p.path("workers.json"), data)
}

func writeAtomic(name string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func ValidHost(host string) bool { return validHost.MatchString(host) }

func (p *Pool) PublicKey() (string, error) {
	key := p.sshPath("id_ed25519")
	if _, err := os.Stat(key); errors.Is(err, os.ErrNotExist) {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "m2test-pool", "-f", key).CombinedOutput(); err != nil {
			return "", fmt.Errorf("ssh-keygen: %v: %s", err, out)
		}
	}
	data, err := os.ReadFile(key + ".pub")
	return strings.TrimSpace(string(data)), err
}

func Fingerprint(key string) (string, error) {
	f := strings.Fields(key)
	if len(f) < 2 {
		return "", fmt.Errorf("not a public key: %q", key)
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// PinHostKey scans a host's keys and returns the one with the fingerprint
// given out of band. A scan alone would trust whoever answered.
func PinHostKey(ctx context.Context, host string, port int, fingerprint string) (string, error) {
	out, err := exec.CommandContext(ctx, "ssh-keyscan", "-T", "10", "-p", strconv.Itoa(port), host).Output()
	if err != nil {
		return "", fmt.Errorf("ssh-keyscan %s: %w", host, err)
	}
	return matchKey(out, fingerprint)
}

func matchKey(scan []byte, fingerprint string) (string, error) {
	var seen []string
	for line := range strings.Lines(string(scan)) {
		f := strings.Fields(line)
		if len(f) != 3 || strings.HasPrefix(f[0], "#") {
			continue
		}
		key := f[1] + " " + f[2]
		fp, err := Fingerprint(key)
		if err != nil {
			continue
		}
		if fp == fingerprint {
			return key, nil
		}
		seen = append(seen, f[1]+" "+fp)
	}
	return "", fmt.Errorf("no host key matches %s; the host offered %v", fingerprint, seen)
}

var Command = func(ctx context.Context, p *Pool, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "ssh", append([]string{"-F", p.sshPath("config")}, args...)...)
}

// runScript saves the script from stdin and runs it with stdin closed, so
// nothing in it shows in a process list and no child eats the rest of it.
// A cut connection ends cat cleanly, so the script must end on scriptEnd.
const (
	scriptEnd = "# m2test end"
	runScript = `d=${XDG_RUNTIME_DIR:-/tmp}; [ -d "$d" ] || d=/tmp; trap 'rm -f "$f"; exit 1' HUP INT TERM; f=$(mktemp -p "$d") && cat > "$f" && [ "$(tail -n 1 "$f")" = "` + scriptEnd + `" ] && sh "$f" </dev/null; r=$?; rm -f "$f"; exit $r`
	// For a script holding a secret: tmpfs or nothing, checked before
	// stdin is read. TMPDIR covers shells that spool heredocs.
	secretScript = `command -v findmnt >/dev/null || exit 96; findmnt -no FSTYPE -T "${XDG_RUNTIME_DIR:?}" | grep -qx tmpfs || exit 97; export TMPDIR="$XDG_RUNTIME_DIR"; ` + runScript
)

func (p *Pool) Script(ctx context.Context, name, script string) ([]byte, error) {
	return p.script(ctx, name, script, runScript)
}

func (p *Pool) script(ctx context.Context, name, script, wrapper string) ([]byte, error) {
	cmd := Command(ctx, p, name, wrapper)
	cmd.Stdin = strings.NewReader(script + "\n" + scriptEnd + "\n")
	stdout, stderr := &tail{max: 1 << 16}, &tail{max: 1 << 16}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", name, err, lastLines(stderr.Bytes(), 5))
	}
	return stdout.Bytes(), nil
}

// tail keeps the last max bytes written, so a noisy or hostile host can't
// fill this machine's memory. Not an embedded Buffer: io.Copy would use its
// ReadFrom and skip the cap.
type tail struct {
	buf bytes.Buffer
	max int
}

func (t *tail) Write(b []byte) (int, error) {
	n := len(b)
	if len(b) > t.max {
		b = b[len(b)-t.max:]
	}
	if over := t.buf.Len() + len(b) - t.max; over > 0 {
		t.buf.Next(over)
	}
	t.buf.Write(b)
	return n, nil
}

func (t *tail) Bytes() []byte  { return t.buf.Bytes() }
func (t *tail) String() string { return t.buf.String() }

// Upload writes a local file to path on a worker, whole or not at all: a
// cut connection ends cat cleanly, so the checksum decides.
func (p *Pool) Upload(ctx context.Context, name, local, path string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	sum, err := fileSHA256(local)
	if err != nil {
		return err
	}
	cmd := Command(ctx, p, name, fmt.Sprintf("cat > '%[1]s.part' && echo '%[2]s  %[1]s.part' | sha256sum -c --quiet - >&2 && mv '%[1]s.part' '%[1]s'", path, sum))
	cmd.Stdin = f
	stderr := &tail{max: 1 << 16}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("uploading to %s:%s: %w: %s", name, path, err, lastLines(stderr.Bytes(), 5))
	}
	return nil
}

func fileSHA256(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// lastLines quotes remote output for a log line, dropping control
// characters so a host can't write terminal escapes into it.
func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i, l := range lines {
		if len(l) > 500 {
			lines[i] = l[:500] + "..."
		}
	}
	s := strings.Join(lines[max(0, len(lines)-n):], " | ")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
