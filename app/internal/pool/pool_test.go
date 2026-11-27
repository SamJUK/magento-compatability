// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/images"
)

const (
	edKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBAgG3bOvLgiuXyClhNeybFo5lMqrdHKuzQe0DihiU+w"
	edPrint = "SHA256:RQQKZzzvtO2RA2rBiuu0qkdR6QCLJBwlleNJaATpm54"
)

func tok() string {
	t, err := NewToken()
	if err != nil {
		panic(err)
	}
	return t
}

func testPool(t *testing.T) *Pool {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func lab() *Worker {
	return &Worker{Host: "192.0.2.30", Port: 22, User: "m2test", Dir: DefaultDir, HostKey: edKey, Token: tok(), Slots: 2}
}

func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	got, err := Fingerprint(edKey)
	if err != nil || got != edPrint {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	cmd := exec.Command("ssh-keygen", "-lf", "-")
	cmd.Stdin = strings.NewReader(edKey + "\n")
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), got) {
		t.Fatalf("ssh-keygen says %s (%v), we say %s", out, err, got)
	}
}

func TestMatchKeyOnlyTakesTheGivenFingerprint(t *testing.T) {
	scan := "# 192.0.2.30:22 SSH-2.0-OpenSSH\n192.0.2.30 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ==\n192.0.2.30 " + edKey + "\n"
	if key, err := matchKey([]byte(scan), edPrint); err != nil || key != edKey {
		t.Fatalf("got %q, %v", key, err)
	}
	if _, err := matchKey([]byte(scan), "SHA256:someoneelse"); err == nil {
		t.Fatal("pinned a key nobody vouched for")
	}
}

func TestSaveWritesConfigAndKnownHostsFromInventory(t *testing.T) {
	p := testPool(t)
	inv := &Inventory{Workers: map[string]*Worker{"lab1": lab(), "local-1": {Token: tok()}}}
	if err := p.Save(inv); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(p.sshPath("config"))
	known, _ := os.ReadFile(p.sshPath("known_hosts"))
	if string(known) != "lab1 "+edKey+"\n" {
		t.Errorf("known hosts: %q", known)
	}
	c := string(cfg)
	for _, want := range []string{"Host lab1\n  HostName 192.0.2.30", "HostKeyAlias lab1", "StrictHostKeyChecking yes", "ForwardAgent no", "IdentitiesOnly yes", "GlobalKnownHostsFile /dev/null"} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	// ssh keeps the first value, so a catch-all before a host would win.
	if strings.Index(c, "Host lab1") > strings.Index(c, "Host *") {
		t.Error("defaults come before the host block")
	}
	if strings.Contains(c, "local-1") {
		t.Error("a local worker got an ssh entry")
	}
	for _, f := range []string{"workers.json", "ssh/config", "ssh/known_hosts"} {
		if fi, err := os.Stat(filepath.Join(p.Dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi.Mode(), err)
		}
	}

	delete(inv.Workers, "lab1")
	if err := p.Save(inv); err != nil {
		t.Fatal(err)
	}
	if known, _ := os.ReadFile(p.sshPath("known_hosts")); len(known) != 0 {
		t.Errorf("removed worker's key kept: %q", known)
	}
	got, err := p.Load()
	if err != nil || len(got.Workers) != 1 || got.Workers["local-1"] == nil {
		t.Fatalf("reload: %v %v", got, err)
	}
}

func TestValidateRefusesWhatWouldReachAShell(t *testing.T) {
	for name, mut := range map[string]func(*Worker){
		"dir quote":   func(w *Worker) { w.Dir = "/var/lib/m2'test" },
		"dir dotdot":  func(w *Worker) { w.Dir = "/var/../etc" },
		"dir rel":     func(w *Worker) { w.Dir = "var/lib" },
		"host opt":    func(w *Worker) { w.Host = "-oProxyCommand=x" },
		"user":        func(w *Worker) { w.User = "m2 test" },
		"token":       func(w *Worker) { w.Token = strings.Repeat("a", 31) + "'" },
		"no host key": func(w *Worker) { w.HostKey = "" },
		"port":        func(w *Worker) { w.Port = 0 },
	} {
		w := lab()
		mut(w)
		if err := w.validate("lab1"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := lab().validate("Lab 1"); err == nil {
		t.Error("bad name accepted")
	}
	if err := lab().validate("lab1"); err != nil {
		t.Errorf("control: %v", err)
	}
}

func needTimeout(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("needs timeout(1)")
	}
}

func fakeSSH(t *testing.T, reply func(args []string) string) *[][]string {
	var mu sync.Mutex
	calls := &[][]string{}
	old := Command
	t.Cleanup(func() { Command = old })
	Command = func(ctx context.Context, _ *Pool, args ...string) *exec.Cmd {
		mu.Lock()
		*calls = append(*calls, args)
		mu.Unlock()
		return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; "+reply(args))
	}
	return calls
}

func TestSetupSendsTheTokenOnStdin(t *testing.T) {
	p := testPool(t)
	w := lab()
	calls := fakeSSH(t, func([]string) string { return "" })
	bin := filepath.Join(t.TempDir(), "bin")
	os.WriteFile(bin, []byte("x"), 0o755)
	if err := p.Setup(context.Background(), "lab1", w, bin); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.Contains(strings.Join(c, " "), w.Token) {
			t.Fatalf("token in ssh args: %v", c)
		}
	}
	if s := setupScript(w); !strings.Contains(s, w.Token) || !strings.Contains(s, "-slots 2") {
		t.Fatalf("script lacks the token or slots:\n%s", s)
	}
}

func TestSetupWaitsForADeploy(t *testing.T) {
	needTimeout(t)
	run := func(lock string) (string, bool) {
		d, bin, home := t.TempDir(), t.TempDir(), t.TempDir()
		os.WriteFile(bin+"/flock", []byte("#!/bin/sh\necho lock >> "+d+"/calls\n"+lock+"\n"), 0o755)
		os.WriteFile(bin+"/systemctl", []byte("#!/bin/sh\n{ true >&9; } 2>/dev/null && echo holds-lock >> "+d+"/calls\necho \"$2\" >> "+d+"/calls\n"), 0o755)
		os.WriteFile(d+"/m2test-worker.new", []byte("new"), 0o644)
		w := lab()
		w.Dir = d
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(setupScript(w))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+home)
		cmd.Run()
		calls, _ := os.ReadFile(d + "/calls")
		_, err := os.Stat(d + "/bin/m2test-worker")
		return string(calls), err == nil
	}
	if calls, swapped := run("exit 0"); calls != "lock\ndaemon-reload\nenable\ntry-restart\n" || !swapped {
		t.Errorf("calls %q, binary swapped %v", calls, swapped)
	}
	if calls, swapped := run("exit 1"); calls != "lock\n" || swapped {
		t.Errorf("lock timed out: calls %q, binary swapped %v", calls, swapped)
	}
}

func TestDeploySkipsUploadWhenTheWorkerHasTheRelease(t *testing.T) {
	p := testPool(t)
	tar := filepath.Join(t.TempDir(), "r.tar.gz")
	os.WriteFile(tar, []byte("x"), 0o644)
	sum, _ := fileSHA256(tar)
	for _, cached := range []string{sum, "", "a corrupt copy"} {
		have := cached == sum
		calls := fakeSSH(t, func(args []string) string {
			if args[len(args)-1] == runScript {
				return "echo '" + cached + "'"
			}
			return ""
		})
		if err := p.Deploy(context.Background(), "lab1", lab(), "abc123", tar); err != nil {
			t.Fatal(err)
		}
		uploads := 0
		for _, c := range *calls {
			if strings.Contains(c[len(c)-1], ".part") {
				uploads++
			}
		}
		if want := map[bool]int{true: 0, false: 1}[have]; uploads != want {
			t.Errorf("cached %q: %d uploads, want %d", cached, uploads, want)
		}
	}
}

func TestDeployScriptRestartsOnlyOnANewRelease(t *testing.T) {
	needTimeout(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	d := t.TempDir()
	bin := t.TempDir()
	os.MkdirAll(d+"/bin", 0o755)
	os.MkdirAll(d+"/releases", 0o755)
	os.MkdirAll(d+"/incoming", 0o755)
	os.WriteFile(d+"/bin/m2test-worker", []byte("#!/bin/sh\necho \"$*\" >> "+d+"/worker\n[ \"$1\" = prune ] || mkdir -p \"$7/$3\"\n"), 0o755)
	// GNU mv's -T, which BSD mv lacks: without it mv moves into the dir a link points to.
	os.WriteFile(bin+"/mv", []byte("#!/bin/sh\n[ \"$1\" = -T ] && { shift; rm -f \"$2\"; }\nexec /bin/mv -f \"$1\" \"$2\"\n"), 0o755)
	os.WriteFile(bin+"/flock", []byte("#!/bin/sh\n"), 0o755)
	// Whatever systemctl starts must not inherit the deploy lock.
	os.WriteFile(bin+"/systemctl", []byte("#!/bin/sh\n{ true >&9; } 2>/dev/null && echo holds-lock >> "+d+"/calls\necho \"$2\" >> "+d+"/calls\n[ \"$2\" = restart ] && rm "+d+"/fail 2>/dev/null && exit 1\nexit 0\n"), 0o755)
	// Never the real daemon: housekeeping prunes images.
	os.WriteFile(bin+"/docker", []byte("#!/bin/sh\necho \"$*\" >> "+d+"/docker\n"), 0o755)
	os.WriteFile(d+"/incoming/abc.tar.gz", []byte("x"), 0o644)
	keep := t.TempDir()
	deploy := func(hash string) ([]byte, error) {
		cmd := exec.Command(sh, "-s")
		cmd.Stdin = strings.NewReader(deployScript(d, hash, ""))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "C="+keep)
		return cmd.CombinedOutput()
	}
	run := func(hash string) {
		if out, err := deploy(hash); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("abc")
	run("abc")
	if _, err := os.Stat(keep); err != nil {
		t.Error("removed a C the shell passed in")
	}
	calls, _ := os.ReadFile(d + "/calls")
	if string(calls) != "restart\nstart\n" {
		t.Errorf("systemctl calls %q", calls)
	}
	if l, _ := os.Readlink(d + "/releases/current"); l != "abc" {
		t.Errorf("current -> %q", l)
	}
	if _, err := os.Stat(d + "/incoming/abc.tar.gz"); err == nil {
		t.Error("upload left in incoming")
	}
	if _, err := os.Stat(d + "/releases/abc.tar.gz"); err != nil {
		t.Error("checked upload not kept as the cached copy")
	}
	docker, _ := os.ReadFile(d + "/docker")
	for _, line := range strings.Split(strings.TrimSpace(string(docker)), "\n") {
		if line != "image prune -f --filter label="+images.ContextLabel && line != "image prune -af --filter label=m2test.release" {
			t.Errorf("housekeeping beyond our own images: docker %s", line)
		}
	}
	worker, _ := os.ReadFile(d + "/worker")
	if !strings.Contains(string(worker), "prune "+d+"/releases/abc\n") {
		t.Errorf("control: images this release doesn't use not pruned: %q", worker)
	}

	// A restart that fails leaves the old release running, so the retry restarts too.
	os.WriteFile(d+"/incoming/def.tar.gz", []byte("x"), 0o644)
	os.WriteFile(d+"/fail", nil, 0o644)
	if _, err := deploy("def"); err == nil {
		t.Fatal("a failed restart deployed")
	}
	run("def")
	calls, _ = os.ReadFile(d + "/calls")
	if string(calls) != "restart\nstart\nrestart\nrestart\n" {
		t.Errorf("after a failed restart, systemctl calls %q", calls)
	}

	// Back to def after ghi failed to restart: current points at ghi, so def restarts.
	os.WriteFile(d+"/incoming/ghi.tar.gz", []byte("x"), 0o644)
	os.WriteFile(d+"/fail", nil, 0o644)
	if _, err := deploy("ghi"); err == nil {
		t.Fatal("a failed restart deployed")
	}
	// Dying before the restart must not leave def's marker for whatever the unit runs.
	if _, err := os.Stat(d + "/running"); err == nil {
		t.Error("running kept through a failed restart")
	}
	run("def")
	calls, _ = os.ReadFile(d + "/calls")
	if string(calls) != "restart\nstart\nrestart\nrestart\nrestart\nrestart\n" {
		t.Errorf("rolling back after a failed restart, systemctl calls %q", calls)
	}
	if l, _ := os.Readlink(d + "/releases/current"); l != "def" {
		t.Errorf("current -> %q after the rollback", l)
	}
}

func TestDeployScriptLendsTheDockerHubTokenOnTmpfsOnly(t *testing.T) {
	needTimeout(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	d, bin, runtime := t.TempDir(), t.TempDir(), t.TempDir()
	for _, sub := range []string{"/bin", "/releases", "/incoming"} {
		os.MkdirAll(d+sub, 0o755)
	}
	// prepare records the config it was given and where.
	// prune, in housekeeping, must not see it.
	os.WriteFile(d+"/bin/m2test-worker", []byte("#!/bin/sh\nif [ \"$1\" = prune ]; then [ -z \"$DOCKER_CONFIG\" ] || echo \"$DOCKER_CONFIG\" >> "+d+"/prune-saw; exit; fi\nmkdir -p \"$7/$3\"\necho \"$DOCKER_CONFIG\" > "+d+"/seen\ncat \"$DOCKER_CONFIG/config.json\" >> "+d+"/seen\n"), 0o755)
	os.WriteFile(bin+"/mv", []byte("#!/bin/sh\n[ \"$1\" = -T ] && shift\nexec /bin/mv -f \"$1\" \"$2\"\n"), 0o755)
	os.WriteFile(bin+"/docker", []byte("#!/bin/sh\n[ -z \"$DOCKER_CONFIG\" ] || echo \"$DOCKER_CONFIG\" >> "+d+"/prune-saw\n"), 0o755)
	for _, stub := range []string{"flock", "systemctl"} {
		os.WriteFile(bin+"/"+stub, []byte("#!/bin/sh\n"), 0o755)
	}
	os.WriteFile(d+"/incoming/abc.tar.gz", []byte("x"), 0o644)
	auth, err := DockerHubAuth("m2test", "dckr_pat_abc")
	if err != nil {
		t.Fatal(err)
	}
	run := func(env ...string) error {
		cmd := exec.Command(sh, "-s")
		cmd.Stdin = strings.NewReader(deployScript(d, "abc", auth))
		cmd.Env = append(os.Environ(), append(env, "PATH="+bin+":"+os.Getenv("PATH"))...)
		return cmd.Run()
	}
	os.Mkdir(runtime+"/m2test-docker.stale", 0o700)
	if err := run("XDG_RUNTIME_DIR=" + runtime); err != nil {
		t.Fatal(err)
	}
	seen, _ := os.ReadFile(d + "/seen")
	lines := strings.SplitN(string(seen), "\n", 2)
	if !strings.HasPrefix(lines[0], runtime+"/m2test-docker.") || !strings.Contains(lines[1], `"auth":"`+auth+`"`) {
		t.Fatalf("prepare saw %q", seen)
	}
	if left, _ := os.ReadDir(runtime); len(left) != 0 {
		t.Errorf("config left behind: %v", left)
	}
	if saw, err := os.ReadFile(d + "/prune-saw"); err == nil {
		t.Errorf("token still lent during housekeeping: %s", saw)
	}
	for _, line := range strings.Split(deployScript(d, "abc", auth), "\n") {
		if strings.Contains(line, auth) && !strings.HasPrefix(line, `{"auths":`) {
			t.Errorf("token outside the heredoc, maybe in argv: %s", line)
		}
	}
	if err := run("XDG_RUNTIME_DIR="); err == nil {
		t.Error("ran with no runtime dir for the token")
	}
	os.WriteFile(d+"/bin/m2test-worker", []byte("#!/bin/sh\nexit 3\n"), 0o755)
	if err := run("XDG_RUNTIME_DIR=" + runtime); err == nil {
		t.Fatal("control: a failed prepare passed")
	}
	if left, _ := os.ReadDir(runtime); len(left) != 0 {
		t.Errorf("config left after a failed prepare: %v", left)
	}
}

func TestDockerHubAuth(t *testing.T) {
	if a, err := DockerHubAuth("", ""); a != "" || err != nil {
		t.Errorf("neither: %q %v", a, err)
	}
	if a, err := DockerHubAuth("m2test", "dckr_pat_x"); err != nil || a != "bTJ0ZXN0OmRja3JfcGF0X3g=" {
		t.Errorf("both: %q %v", a, err)
	}
	for _, bad := range [][2]string{{"m2test", ""}, {"", "tok"}, {"x'; reboot", "tok"}, {"m2test", "a b"}} {
		if _, err := DockerHubAuth(bad[0], bad[1]); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestTunnelReconnectsWithBackoffUntilCancelled(t *testing.T) {
	p := testPool(t)
	old := Backoff
	Backoff = [2]time.Duration{20 * time.Millisecond, 80 * time.Millisecond}
	t.Cleanup(func() { Backoff = old })
	var mu sync.Mutex
	var at []time.Time
	ctx, cancel := context.WithCancel(context.Background())
	fakeSSH(t, func(args []string) string {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if len(at) == 6 {
			cancel()
		}
		return "exit 255"
	})
	done := make(chan struct{})
	go func() {
		p.Tunnel(ctx, "lab1", lab(), "/tmp/w.sock", log.New(io.Discard, "", 0))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 6 {
		t.Fatalf("%d attempts", len(at))
	}
	// 20, 40, 80, 80, 80 ms: doubled, then capped (uncapped, the last is 320).
	if gap := at[5].Sub(at[4]); gap < 80*time.Millisecond || gap > 250*time.Millisecond {
		t.Errorf("capped gap %s", gap)
	}
	if gap := at[1].Sub(at[0]); gap > at[3].Sub(at[2]) {
		t.Errorf("no backoff: first gap %s, later %s", gap, at[3].Sub(at[2]))
	}
}

// attemptClock makes each clock reading a second after the last, so
// RefusedFor spans a fixed number of attempts however slow the machine.
func attemptClock(t *testing.T) {
	var now time.Time
	old := clock
	t.Cleanup(func() { clock = old })
	clock = func() time.Time { now = now.Add(time.Second); return now }
}

func TestTunnelGivesUpOnlyOnSustainedRefusal(t *testing.T) {
	p := testPool(t)
	old, oldFor := Backoff, RefusedFor
	Backoff, RefusedFor = [2]time.Duration{time.Millisecond, 4 * time.Millisecond}, 10*time.Second
	attemptClock(t)
	t.Cleanup(func() { Backoff, RefusedFor = old, oldFor })
	refuse := "echo 'Error: remote port forwarding failed for listen path /x' >&2; exit 255"
	wrongKey := "echo 'Host key verification failed.' >&2; exit 255"
	denied := "echo 'git@lab1: Permission denied (password).' >&2; exit 255"
	var mu sync.Mutex
	n := 0
	for _, tc := range []struct {
		name   string
		reply  func(int) string
		giveUp bool
	}{
		{"refused", func(int) string { return refuse }, true},
		{"wrong host key", func(int) string { return wrongKey }, true},
		{"our key refused", func(int) string { return denied }, true},
		{"wrong host key then refused", func(i int) string {
			if (i/4)%2 == 0 {
				return wrongKey
			}
			return refuse
		}, false},
		// A dropped connection between refusals starts the clock again.
		{"interleaved", func(i int) string {
			if i%3 == 0 {
				return "exit 255"
			}
			return refuse
		}, false},
	} {
		n = 0
		// Ended by attempts, not time: giving up takes about five.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		fakeSSH(t, func([]string) string {
			mu.Lock()
			defer mu.Unlock()
			if n++; n > 30 {
				cancel()
			}
			return tc.reply(n)
		})
		start := time.Now()
		err := p.Tunnel(ctx, "lab1", lab(), "/tmp/w.sock", log.New(io.Discard, "", 0))
		cancel()
		if gaveUp := err != nil; gaveUp != tc.giveUp {
			t.Errorf("%s: gave up %v after %s: %v", tc.name, gaveUp, time.Since(start), err)
		}
	}
}

func TestTunnelForwardsToTheWorkerDir(t *testing.T) {
	p := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := fakeSSH(t, func([]string) string { cancel(); return "exit 1" })
	p.Tunnel(ctx, "lab1", lab(), "/state/worker.sock", log.New(io.Discard, "", 0))
	got := strings.Join((*calls)[0], " ")
	if got != "-N -R /var/lib/m2test/worker.sock:/state/worker.sock lab1" {
		t.Errorf("ssh %s", got)
	}
}

func TestInventoryRefusesSharedTokensAndHosts(t *testing.T) {
	a, b := lab(), lab()
	for name, inv := range map[string]*Inventory{
		"token": {Workers: map[string]*Worker{"a": {Token: a.Token}, "b": {Token: a.Token}}},
		"host":  {Workers: map[string]*Worker{"a": a, "b": b}},
		"null":  {Workers: map[string]*Worker{"a": nil}},
		"dirs":  {Workers: map[string]*Worker{"a": lab(), "b": func() *Worker { w := lab(); w.Dir = "/srv/m2test"; return w }()}},
		"alias": {Workers: map[string]*Worker{"a": lab(), "b": func() *Worker { w := lab(); w.Host = "lab.example"; return w }()}},
	} {
		if err := inv.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	b.Host, b.HostKey = "192.0.2.31", "ssh-ed25519 AAAAOTHER"
	if err := (&Inventory{Workers: map[string]*Worker{"a": a, "b": b}}).validate(); err != nil {
		t.Errorf("control: %v", err)
	}
}

func TestTailKeepsOnlyTheEnd(t *testing.T) {
	tl := &tail{max: 8}
	for _, s := range []string{"0123", "456789", "abcdefghijklmnop", "XY"} {
		if n, _ := tl.Write([]byte(s)); n != len(s) {
			t.Fatalf("short write %d", n)
		}
	}
	if got := string(tl.Bytes()); got != "klmnopXY" {
		t.Errorf("kept %q", got)
	}
}

func localSSH(t *testing.T, filter string) {
	old := Command
	t.Cleanup(func() { Command = old })
	Command = func(ctx context.Context, _ *Pool, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", filter+" | ( "+args[len(args)-1]+" )")
	}
}

func TestUploadRefusesATruncatedFile(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	p := testPool(t)
	src := filepath.Join(t.TempDir(), "bin")
	os.WriteFile(src, []byte("the whole binary"), 0o644)
	dst := filepath.Join(t.TempDir(), "m2test-worker")
	localSSH(t, "head -c 5")
	if err := p.Upload(context.Background(), "lab1", src, dst); err == nil {
		t.Error("truncated upload accepted")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("truncated file installed")
	}
	localSSH(t, "cat")
	if err := p.Upload(context.Background(), "lab1", src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "the whole binary" {
		t.Errorf("got %q", b)
	}
}

func TestScriptChildrenCannotEatTheScript(t *testing.T) {
	p := testPool(t)
	localSSH(t, "cat")
	out, err := p.Script(context.Background(), "lab1", "cat >/dev/null\necho after\n")
	if err != nil || strings.TrimSpace(string(out)) != "after" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestDeployUntilDoneRetries(t *testing.T) {
	p := testPool(t)
	old := Backoff
	Backoff = [2]time.Duration{time.Millisecond, 4 * time.Millisecond}
	t.Cleanup(func() { Backoff = old })
	tar := filepath.Join(t.TempDir(), "r.tar.gz")
	os.WriteFile(tar, []byte("x"), 0o644)
	n := 0
	fakeSSH(t, func([]string) string {
		n++
		if n < 4 {
			return "exit 255"
		}
		return ""
	})
	if err := p.DeployUntilDone(context.Background(), "lab1", lab(), "abc", tar, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("gave up: %v", err)
	}
	if n < 4 {
		t.Errorf("%d calls", n)
	}
}

func TestDeployUntilDoneGivesUpOnASustainedRefusal(t *testing.T) {
	p := testPool(t)
	old, oldFor := Backoff, RefusedFor
	Backoff, RefusedFor = [2]time.Duration{time.Millisecond, 4 * time.Millisecond}, 10*time.Second
	attemptClock(t)
	t.Cleanup(func() { Backoff, RefusedFor = old, oldFor })
	tar := filepath.Join(t.TempDir(), "r.tar.gz")
	os.WriteFile(tar, []byte("x"), 0o644)
	wrongKey := "echo 'Host key verification failed.' >&2; exit 255"
	denied := "echo 'git@lab1: Permission denied (password).' >&2; exit 255"
	for _, tc := range []struct {
		name   string
		reply  func(int) string
		giveUp bool
	}{
		{"wrong host key", func(int) string { return wrongKey }, true},
		{"our key refused", func(int) string { return denied }, true},
		// A dropped connection between them starts the clock again.
		{"interleaved", func(i int) string {
			if i%3 == 0 {
				return "exit 255"
			}
			return wrongKey
		}, false},
	} {
		n := 0
		// Ended by attempts, not time: giving up takes about five.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		fakeSSH(t, func([]string) string {
			if n++; n > 30 {
				cancel()
			}
			return tc.reply(n)
		})
		err := p.DeployUntilDone(ctx, "lab1", lab(), "abc", tar, log.New(io.Discard, "", 0))
		cancel()
		if gaveUp := errors.Is(err, ErrUnfit); gaveUp != tc.giveUp {
			t.Errorf("%s: gave up %v: %v", tc.name, gaveUp, err)
		}
	}
}

func TestScriptRefusesATruncatedScript(t *testing.T) {
	p := testPool(t)
	localSSH(t, "head -c 20")
	out, err := p.Script(context.Background(), "lab1", "echo ran\necho 0123456789\n")
	if err == nil || strings.Contains(string(out), "ran") {
		t.Fatalf("ran a cut script: %q %v", out, err)
	}
}

func TestValidateRefusesUncleanDirs(t *testing.T) {
	for _, d := range []string{"/var/lib/m2test/", "/var/lib/./m2test", "/var//lib"} {
		w := lab()
		w.Dir = d
		if w.validate("lab1") == nil {
			t.Errorf("%s accepted", d)
		}
	}
}

func TestTailCapsCommandOutput(t *testing.T) {
	tl := &tail{max: 1 << 10}
	cmd := exec.Command("sh", "-c", "head -c 1000000 /dev/zero")
	cmd.Stdout = tl
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if n := len(tl.Bytes()); n != 1<<10 {
		t.Errorf("kept %d bytes", n)
	}
}

func TestValidateBoundsRemoteSlots(t *testing.T) {
	for _, n := range []int{0, -1, MaxSlots + 1} {
		w := lab()
		w.Slots = n
		if err := w.validate("lab1"); err == nil {
			t.Errorf("slots %d accepted", n)
		}
	}
	for _, n := range []int{1, MaxSlots} {
		w := lab()
		w.Slots = n
		if err := w.validate("lab1"); err != nil {
			t.Errorf("slots %d: %v", n, err)
		}
	}
	if err := (&Worker{Token: tok()}).validate("local"); err != nil {
		t.Errorf("local worker without slots: %v", err)
	}
}

func TestLockRefusesASecondSweep(t *testing.T) {
	p := &Pool{Dir: t.TempDir()}
	os.MkdirAll(filepath.Join(p.Dir, "ssh"), 0o700)
	f, err := p.Lock("lab1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Lock("lab1"); err == nil {
		t.Fatal("second lock taken while the first is held")
	}
	other, err := p.Lock("lab2")
	if err != nil {
		t.Fatalf("other worker: %v", err)
	}
	other.Close()
	// A worker named workers doesn't hold the inventory's lock.
	named, err := p.Lock("workers")
	if err != nil {
		t.Fatal(err)
	}
	updated := make(chan error, 1)
	go func() { updated <- p.Update(func(*Inventory) error { return nil }) }()
	select {
	case err := <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker workers's lock blocks the inventory")
	}
	named.Close()
	f.Close()
	if f, err = p.Lock("lab1"); err != nil {
		t.Fatalf("lock not freed: %v", err)
	}
	f.Close()
}

func TestValidateRefusesADirTooLongForTheSocket(t *testing.T) {
	w := lab()
	w.Dir = "/" + strings.Repeat("d", 107-len("/worker.sock")-1)
	if err := w.validate("lab1"); err != nil {
		t.Fatalf("longest dir refused: %v", err)
	}
	w.Dir += "d"
	if err := w.validate("lab1"); err == nil {
		t.Fatal("dir too long for worker.sock accepted")
	}
}

func TestLastLinesCapsEachLine(t *testing.T) {
	got := LastLines([]byte("short\n"+strings.Repeat("x", 70000)), 5)
	if len(got) > 600 || !strings.HasPrefix(got, "short | xxx") {
		t.Fatalf("got %d bytes: %.40q", len(got), got)
	}
}

func TestValidateCloudEntries(t *testing.T) {
	creating := func() *Worker {
		w := lab()
		w.Host, w.Cloud = "", &Cloud{Provider: "hetzner", Nonce: "abcdef12"}
		return w
	}
	if err := creating().validate("hz-1"); err != nil {
		t.Fatalf("a server not yet answered: %v", err)
	}
	made := creating()
	made.Host, made.Cloud.ID = "203.0.113.7", "42"
	if err := made.validate("hz-1"); err != nil {
		t.Fatalf("a made server: %v", err)
	}
	made.Cloud.Provider, made.Cloud.Region, made.Cloud.Owner, made.Cloud.ID = "aws", "eu-west-1", "111111111111", "i-0123456789abcdef0"
	if err := made.validate("aws-1"); err != nil {
		t.Fatalf("a made instance: %v", err)
	}
	for name, edit := range map[string]func(*Worker){
		"id without address": func(w *Worker) { w.Cloud.ID = "42" },
		"provider":           func(w *Worker) { w.Cloud.Provider = "gcp" },
		"aws without region": func(w *Worker) { w.Cloud.Provider = "aws" },
		"aws bad region": func(w *Worker) {
			w.Cloud.Provider, w.Cloud.Region, w.Cloud.Owner = "aws", "eu-west-1; x", "111111111111"
		},
		"hetzner region":     func(w *Worker) { w.Cloud.Region = "eu-west-1" },
		"region in provider": func(w *Worker) { w.Cloud.Provider = "aws eu-west-1" },
		"aws bad account":    func(w *Worker) { w.Cloud.Provider, w.Cloud.Region, w.Cloud.Owner = "aws", "eu-west-1", "12345" },
		"hetzner account":    func(w *Worker) { w.Cloud.Owner = "key-7" },
		"id":                 func(w *Worker) { w.Host, w.Cloud.ID = "203.0.113.7", "../42" },
		"nonce":              func(w *Worker) { w.Cloud.Nonce = "x; reboot" },
		"no host key":        func(w *Worker) { w.HostKey = "" },
	} {
		w := creating()
		edit(w)
		if err := w.validate("hz-1"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestUpdateLosesNoConcurrentChange(t *testing.T) {
	p := &Pool{Dir: t.TempDir()}
	os.MkdirAll(filepath.Join(p.Dir, "ssh"), 0o700)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			err := p.Update(func(inv *Inventory) error {
				inv.Workers[fmt.Sprintf("w%d", i)] = &Worker{Token: tok()}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	inv, err := p.Load()
	if err != nil || len(inv.Workers) != 20 {
		t.Fatalf("%d workers, %v", len(inv.Workers), err)
	}
}

func TestHostKeyPair(t *testing.T) {
	priv, pub, err := HostKeyPair("hz-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(priv, "-----BEGIN OPENSSH PRIVATE KEY-----") || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("got %q / %q", priv[:20], pub)
	}
	f := strings.Fields(pub)
	if !validKey.MatchString(f[0] + " " + f[1]) {
		t.Fatalf("pinned form %q not valid", pub)
	}
	if _, pub2, _ := HostKeyPair("hz-1"); pub2 == pub {
		t.Fatal("two servers got one key")
	}
}

func TestIDIsMadeOnceAndKept(t *testing.T) {
	p := &Pool{Dir: t.TempDir()}
	a, err := p.ID()
	if err != nil || len(a) != 12 {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := p.ID(); b != a {
		t.Fatalf("second call %q, first %q", b, a)
	}
	if b, _ := (&Pool{Dir: t.TempDir()}).ID(); b == a {
		t.Fatal("control: another pool got the same ID")
	}
}

func TestDeployWithATokenGoesThroughTheTmpfsWrapper(t *testing.T) {
	p := testPool(t)
	p.DockerAuth, _ = DockerHubAuth("m2test", "dckr_pat_abc")
	w := lab()
	tarball := filepath.Join(t.TempDir(), "abc.tar.gz")
	os.WriteFile(tarball, []byte("x"), 0o644)
	var wrappers []string
	fakeSSH(t, func(args []string) string {
		wrappers = append(wrappers, args[len(args)-1])
		return ""
	})
	if err := p.Deploy(context.Background(), "lab1", w, "abc", tarball); err != nil {
		t.Fatal(err)
	}
	if last := wrappers[len(wrappers)-1]; last != secretScript {
		t.Fatalf("deploy with the token ran under %q", last)
	}
	p.DockerAuth = ""
	wrappers = nil
	if err := p.Deploy(context.Background(), "lab1", w, "abc", tarball); err != nil || wrappers[len(wrappers)-1] != runScript {
		t.Fatalf("control: plain deploy ran under %q, %v", wrappers, err)
	}
}

func TestSecretScriptReadsNothingWithoutATmpfsRuntimeDir(t *testing.T) {
	bin, runtime := t.TempDir(), t.TempDir()
	run := func(fstype, xdg string) (string, error) {
		os.WriteFile(bin+"/findmnt", []byte("#!/bin/sh\necho "+fstype+"\n"), 0o755)
		cmd := exec.Command("sh", "-c", secretScript)
		cmd.Stdin = strings.NewReader("echo ran \"$TMPDIR\"\n" + scriptEnd + "\n")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "XDG_RUNTIME_DIR="+xdg, "TMPDIR=/tmp")
		out, err := cmd.Output()
		return string(out), err
	}
	for _, c := range [][2]string{{"ext4", runtime}, {"tmpfs", ""}} {
		// 97 is permanent: anything else is retried.
		if out, err := run(c[0], c[1]); out != "" || !isExit(err, 97) {
			t.Errorf("%v: %v (%q), want exit 97", c, err, out)
		}
	}
	if out, err := run("tmpfs", runtime); err != nil || out != "ran "+runtime+"\n" {
		t.Fatalf("control: %q %v", out, err)
	}
	if left, _ := os.ReadDir(runtime); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestScriptsFallBackToTmpWhenTheRuntimeDirIsMissing(t *testing.T) {
	cmd := exec.Command("sh", "-c", runScript)
	cmd.Stdin = strings.NewReader("echo ran\n" + scriptEnd + "\n")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+t.TempDir()+"/gone")
	if out, err := cmd.Output(); err != nil || string(out) != "ran\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestTokenDeployBlamesTmpfsOnlyWhenTheWrapperSaysSo(t *testing.T) {
	p := testPool(t)
	p.DockerAuth, _ = DockerHubAuth("m2test", "dckr_pat_abc")
	tarball := filepath.Join(t.TempDir(), "abc.tar.gz")
	os.WriteFile(tarball, []byte("x"), 0o644)
	for code, want := range map[string]bool{"exit 97": true, "exit 96": true, "exit 1": false} {
		fakeSSH(t, func(args []string) string {
			if args[len(args)-1] == secretScript {
				return code
			}
			return ""
		})
		err := p.Deploy(context.Background(), "lab1", lab(), "abc", tarball)
		if err == nil || strings.Contains(err.Error(), "XDG_RUNTIME_DIR") != want || errors.Is(err, ErrUnfit) != want || errors.Is(err, ErrPermanent) {
			t.Errorf("%s: %v", code, err)
		}
	}
}

func TestDeployGivesUpOnAnUnpublishedImage(t *testing.T) {
	p := testPool(t)
	tarball := filepath.Join(t.TempDir(), "abc.tar.gz")
	os.WriteFile(tarball, []byte("x"), 0o644)
	for _, auth := range []string{"", "dckr_pat_abc"} {
		p.DockerAuth = ""
		if auth != "" {
			p.DockerAuth, _ = DockerHubAuth("m2test", auth)
		}
		for code, want := range map[string]bool{"exit 98": true, "exit 1": false} {
			scripts := 0
			fakeSSH(t, func(args []string) string {
				// The first asks for the tarball's checksum; the deploy is next.
				if w := args[len(args)-1]; w == runScript || w == secretScript {
					if scripts++; scripts == 2 {
						return code
					}
				}
				return ""
			})
			err := p.Deploy(context.Background(), "lab1", lab(), "abc", tarball)
			if err == nil || errors.Is(err, ErrPermanent) != want {
				t.Errorf("token %v, %s: %v", auth != "", code, err)
			}
		}
	}
}

func TestScriptsCleanUpWhenTheSessionIsHungUp(t *testing.T) {
	runtime := t.TempDir()
	cmd := exec.Command("sh", "-c", runScript)
	cmd.Stdin = strings.NewReader("sleep 30\n" + scriptEnd + "\n")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+runtime)
	// sshd hangs up the session's whole process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Written, not just created: until mktemp returns the trap has no name to remove.
	saved := func() bool {
		left, _ := os.ReadDir(runtime)
		if len(left) != 1 {
			return false
		}
		info, err := left[0].Info()
		return err == nil && info.Size() > 0
	}
	for deadline := time.Now().Add(5 * time.Second); !saved() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if !saved() {
		left, _ := os.ReadDir(runtime)
		t.Fatalf("control: script not saved: %v", left)
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	cmd.Wait()
	if left, _ := os.ReadDir(runtime); len(left) != 0 {
		t.Errorf("script left behind: %v", left)
	}
}

func TestServerIDsFromOlderInventoriesLoad(t *testing.T) {
	var c Cloud
	if err := json.Unmarshal([]byte(`{"provider":"hetzner","id":42,"nonce":"abcdef12"}`), &c); err != nil || c.ID != "42" {
		t.Fatalf("number: %q %v", c.ID, err)
	}
	if err := json.Unmarshal([]byte(`{"provider":"aws","id":"i-0123456789abcdef0","nonce":"abcdef12"}`), &c); err != nil || c.ID != "i-0123456789abcdef0" {
		t.Fatalf("string: %q %v", c.ID, err)
	}
}

func TestDeployUntilDoneStopsOnAnUnfitHost(t *testing.T) {
	p := testPool(t)
	p.DockerAuth, _ = DockerHubAuth("m2test", "dckr_pat_abc")
	tar := filepath.Join(t.TempDir(), "r.tar.gz")
	os.WriteFile(tar, []byte("x"), 0o644)
	n := 0
	fakeSSH(t, func(args []string) string {
		if args[len(args)-1] == secretScript {
			n++
			return "exit 97"
		}
		return ""
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.DeployUntilDone(ctx, "lab1", lab(), "abc", tar, log.New(io.Discard, "", 0)); !errors.Is(err, ErrUnfit) || n != 1 {
		t.Fatalf("%v after %d tries", err, n)
	}
}

func TestDeployPrunesByTheHarnessLabel(t *testing.T) {
	if !strings.Contains(deployScript("/d", "h", ""), "--filter label="+images.ContextLabel+"\n") {
		t.Error("prune filter doesn't match images.ContextLabel")
	}
}

func TestStopWaitsForADeploy(t *testing.T) {
	needTimeout(t)
	p := testPool(t)
	localSSH(t, "cat")
	stop := func(lock string) (string, error) {
		d, bin := t.TempDir(), t.TempDir()
		os.WriteFile(bin+"/flock", []byte("#!/bin/sh\necho lock >> "+d+"/calls\n"+lock+"\n"), 0o755)
		os.WriteFile(bin+"/systemctl", []byte("#!/bin/sh\necho \"$2\" >> "+d+"/calls\n"), 0o755)
		t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
		w := lab()
		w.Dir = d
		err := p.Stop(context.Background(), "lab1", w)
		calls, _ := os.ReadFile(d + "/calls")
		return string(calls), err
	}
	if calls, err := stop("exit 0"); err != nil || calls != "lock\ndisable\n" {
		t.Errorf("calls %q, %v; want the deploy lock first", calls, err)
	}
	if calls, err := stop("exit 1"); err == nil || calls != "lock\n" {
		t.Errorf("lock timed out: calls %q, %v; want no disable", calls, err)
	}
}

func isExit(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

func TestDeployScriptEndsWithinTheAttempt(t *testing.T) {
	s := deployScript("/d", "h", "")
	lock := regexp.MustCompile(`timeout (\d+) flock`).FindStringSubmatch(s)
	prep := regexp.MustCompile(`timeout (\d+)m "\$D/bin/m2test-worker" prepare`).FindStringSubmatch(s)
	if lock == nil || prep == nil {
		t.Fatalf("no lock or prepare timeout in:\n%s", s)
	}
	l, _ := strconv.Atoi(lock[1])
	p, _ := strconv.Atoi(prep[1])
	if total := time.Duration(l)*time.Second + time.Duration(p)*time.Minute; total >= deployAttempt {
		t.Errorf("lock %ds + prepare %dm = %s, not under the %s attempt", l, p, total, deployAttempt)
	}
	if p < 30 {
		t.Errorf("prepare gets %dm; a wide release took over 20m on a cx33", p)
	}
	if total := time.Duration(l)*time.Second + time.Duration(p)*time.Minute; deployAttempt-total < 10*time.Minute {
		t.Errorf("only %s of the attempt left for the upload", deployAttempt-total)
	}
	// Setup and Stop wait out a deploy's prepare, then its restart.
	held := regexp.MustCompile(`timeout (\d+) flock`).FindStringSubmatch(setupScript(&Worker{Dir: "/d"}))
	if w, _ := strconv.Atoi(held[1]); time.Duration(w)*time.Second < time.Duration(p)*time.Minute+5*time.Minute {
		t.Errorf("setup waits %ds for the lock; a deploy can hold it for %dm and more", w, p)
	}
}

func TestLockRefusesWhatIsntAWorkerName(t *testing.T) {
	p := testPool(t)
	if l, err := p.Lock("Lab1"); err == nil {
		l.Close()
		t.Error("locked Lab1")
	}
	l, err := p.Lock("lab1")
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	l.Close()
}
