package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/queue"
)

func TestMain(m *testing.M) {
	if args := os.Getenv("MAIN_TEST_BUILDER_ARGS"); args != "" {
		os.Args = append([]string{"builder"}, strings.Split(args, "\n")...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// builder runs this binary as the builder from the repo root, with a docker
// that always fails so nothing reaches a registry.
func builder(t *testing.T, state string, args ...string) *exec.Cmd {
	t.Helper()
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	cmd := exec.Command(os.Args[0])
	cmd.Dir = "../../.."
	cmd.Env = append(os.Environ(), "MAIN_TEST_BUILDER_ARGS="+strings.Join(args, "\n"), "XDG_STATE_HOME="+state,
		"PATH="+bin+":"+os.Getenv("PATH"), "CI=1")
	return cmd
}

// withResults writes a result for every combination args selects.
func withResults(t *testing.T, results string, args ...string) []string {
	t.Helper()
	out, err := builder(t, t.TempDir(), append([]string{"test", "-results-dir", results, "-list-json"}, args...)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	var combos []struct {
		ID      string `json:"id"`
		Product string `json:"product"`
	}
	if err := json.Unmarshal(out, &combos); err != nil || len(combos) == 0 {
		t.Fatalf("no combinations (%v): %s", err, out)
	}
	ids := make([]string, len(combos))
	for i, c := range combos {
		ids[i] = c.ID
		os.MkdirAll(filepath.Join(results, c.Product), 0o755)
		os.WriteFile(filepath.Join(results, c.Product, c.ID+".json"), []byte("{}"), 0o644)
	}
	return ids
}

func TestTest_ARerunWithEveryResultInNeedsNoImages(t *testing.T) {
	results := t.TempDir()
	withResults(t, results, "-baseline", "-product", "magento")
	out, err := builder(t, t.TempDir(), "test", "-baseline", "-product", "magento", "-results-dir", results).CombinedOutput()
	if err != nil || strings.Contains(string(out), "Readying") {
		t.Errorf("readied images for nothing to run (%v): %s", err, out)
	}
}

func TestTest_ASkipLeavesTheQueuesLeaseAlone(t *testing.T) {
	results, state := t.TempDir(), t.TempDir()
	ids := withResults(t, results, "-baseline", "-product", "magento")
	lg, err := queue.OpenLedger(filepath.Join(state, "magento-compatibility", "queue"))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	for _, id := range ids {
		if ok, err := lg.Lease(results, id); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	out, err := builder(t, state, "test", "-baseline", "-product", "magento", "-results-dir", results).CombinedOutput()
	if err != nil || strings.Contains(string(out), "a queue is running it") {
		t.Errorf("took a lease for runs it skips (%v): %s", err, out)
	}
}

func TestTest_ARunAQueueHoldsIsNotAPass(t *testing.T) {
	for _, sel := range [][]string{{"-baseline", "-product", "magento"}, {"-product", "magento", "-version", "2.4.8"}} {
		results, state := t.TempDir(), t.TempDir()
		ids := withResults(t, results, sel...)
		lg, err := queue.OpenLedger(filepath.Join(state, "magento-compatibility", "queue"))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if ok, err := lg.Lease(results, id); !ok || err != nil {
				t.Fatal(ok, err)
			}
		}
		out, err := builder(t, state, append([]string{"test", "-results-dir", results, "-compose-dir", t.TempDir(), "-force"}, sel...)...).CombinedOutput()
		lg.Close()
		if !strings.Contains(string(out), "a queue is running it; skipped") {
			t.Fatalf("%v: never reached the lease (%v): %s", sel, err, out)
		}
		if err == nil {
			t.Errorf("%v: exited 0 with every run skipped: %s", sel, out)
		}
	}
}

func TestQueue_SlotsZeroAloneSavesNoSweep(t *testing.T) {
	state := t.TempDir()
	out, err := builder(t, state, "queue", "-slots", "0", "-baseline", "-product", "magento", "-results-dir", t.TempDir()).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "-slots 0 runs nothing here") {
		t.Fatalf("not refused (%v): %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(state, "magento-compatibility", "queue", "ledger.json")); err == nil {
		t.Error("a refused start saved a sweep")
	}
}

func TestTest_AnInterruptedRunIsNotAPass(t *testing.T) {
	for _, sel := range [][]string{{"-baseline", "-product", "magento"}, {"-product", "magento", "-version", "2.4.8"}} {
		results, bin := t.TempDir(), t.TempDir()
		withResults(t, results, sel...)
		started := filepath.Join(bin, "started")
		// Stack startup hangs until the signal; everything else succeeds.
		os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\ncase \" $* \" in *\" up \"*) touch \"$STARTED\"; exec sleep 30;; esac\n"), 0o755)
		cmd := builder(t, t.TempDir(), append([]string{"test", "-results-dir", results, "-compose-dir", t.TempDir(), "-force", "-concurrency", "1"}, sel...)...)
		cmd.Env = append(cmd.Env, "PATH="+bin+":"+os.Getenv("PATH"), "STARTED="+started)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				break
			}
		}
		if _, err := os.Stat(started); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("%v: never reached stack startup: %s", sel, out.String())
		}
		cmd.Process.Signal(syscall.SIGTERM)
		if err := cmd.Wait(); err == nil {
			t.Errorf("%v: exited 0 after an interrupt: %s", sel, out.String())
		}
	}
}

func TestQueue_AnInterruptedSweepIsNotAPass(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	// The disk guard holds every run until the signal.
	cmd := builder(t, t.TempDir(), "queue", "-baseline", "-product", "magento", "-results-dir", t.TempDir(), "-compose-dir", t.TempDir(), "-min-free-gb", "1000000000")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	log := func() string { data, _ := os.ReadFile(logPath); return string(data) }
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(log(), "started:") && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	err = cmd.Wait()
	if !strings.Contains(log(), "stopped") {
		t.Fatalf("control: never stopped (%v): %s", err, log())
	}
	if err == nil {
		t.Errorf("exited 0 after an interrupt: %s", log())
	}
}

func TestQueue_ASweepLeftWithoutResultsIsNotAPass(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		results, ids := t.TempDir(), filepath.Join(t.TempDir(), "ids")
		os.WriteFile(ids, []byte(withResults(t, results, "-product", "magento", "-version", "2.4.8")[0]+"\n"), 0o644)
		os.RemoveAll(filepath.Join(results, "magento"))
		if blocked {
			// No result can be written.
			os.WriteFile(filepath.Join(results, "magento"), nil, 0o644)
		}
		out, err := builder(t, t.TempDir(), "queue", "-ids-file", ids, "-results-dir", results, "-compose-dir", t.TempDir(), "-min-free-gb", "0").CombinedOutput()
		if !strings.Contains(string(out), "done: ") {
			t.Fatalf("blocked %v: never finished (%v): %s", blocked, err, out)
		}
		if failed := err != nil; failed != blocked {
			t.Errorf("blocked %v: exit %v: %s", blocked, err, out)
		}
	}
}
