// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package pool

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	d := t.TempDir()
	bin := t.TempDir()
	os.MkdirAll(d+"/bin", 0o755)
	os.MkdirAll(d+"/releases", 0o755)
	os.MkdirAll(d+"/incoming", 0o755)
	os.WriteFile(d+"/bin/m2test-worker", []byte("#!/bin/sh\nmkdir -p \"$7/$3\"\n"), 0o755)
	// GNU mv's -T; the test may run where mv lacks it.
	os.WriteFile(bin+"/mv", []byte("#!/bin/sh\n[ \"$1\" = -T ] && shift\nexec /bin/mv -f \"$1\" \"$2\"\n"), 0o755)
	os.WriteFile(bin+"/flock", []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(bin+"/systemctl", []byte("#!/bin/sh\necho \"$2\" >> "+d+"/calls\n"), 0o755)
	os.WriteFile(d+"/incoming/abc.tar.gz", []byte("x"), 0o644)
	run := func(hash string) {
		cmd := exec.Command(sh, "-s")
		cmd.Stdin = strings.NewReader(deployScript(d, hash))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("abc")
	run("abc")
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
	if got := tl.String(); got != "klmnopXY" {
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
	if !p.DeployUntilDone(context.Background(), "lab1", lab(), "abc", tar, log.New(io.Discard, "", 0)) {
		t.Fatal("gave up")
	}
	if n < 4 {
		t.Errorf("%d calls", n)
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
	got := lastLines([]byte("short\n"+strings.Repeat("x", 70000)), 5)
	if len(got) > 600 || !strings.HasPrefix(got, "short | xxx") {
		t.Fatalf("got %d bytes: %.40q", len(got), got)
	}
}
