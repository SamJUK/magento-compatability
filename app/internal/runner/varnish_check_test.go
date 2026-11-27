// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package runner

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

func TestServiceChecksOnlyIncludeVarnishWhenEnabled(t *testing.T) {
	hasVarnish := func(c matrix.Combination) bool {
		for _, check := range serviceChecks(c) {
			if check.name == "varnish" {
				return true
			}
		}
		return false
	}

	if !hasVarnish(matrix.Combination{Varnish: "7.7"}) {
		t.Fatal("no varnish check with Varnish 7.7")
	}
	for _, none := range []string{"none", ""} {
		if hasVarnish(matrix.Combination{Varnish: none}) {
			t.Fatalf("varnish check with Varnish %q", none)
		}
	}
}

func fastBackendWait(t *testing.T) {
	t.Helper()
	timeout, interval := varnishBackendTimeout, varnishBackendInterval
	varnishBackendTimeout, varnishBackendInterval = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { varnishBackendTimeout, varnishBackendInterval = timeout, interval })
}

func TestRunVarnishCheckLoadsTheVCLBetweenConfigureAndCheck(t *testing.T) {
	fastBackendWait(t)
	var calls []string
	cp := &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			joined := strings.Join(args, " ")
			calls = append(calls, joined)
			if strings.Contains(joined, "backend.list") {
				return exec.CommandContext(ctx, "echo", "magento.default probe 5/5 healthy")
			}
			return exec.CommandContext(ctx, "true")
		},
	}

	status, log := runVarnishCheck(context.Background(), cp)

	if status != result.StatusPass {
		t.Fatalf("status = %q, want pass; log:\n%s", status, log)
	}
	wantOrder := []string{
		"check-varnish.sh",
		"cp php-fpm:/scripts/tests/varnish.vcl",
		"cp ",
		"varnishadm vcl.load magento /etc/varnish/magento.vcl",
		"varnishadm vcl.use magento",
		"varnishadm backend.list",
		"varnishadm ban req.url ~ .",
		"check-varnish.php",
	}
	if len(calls) != len(wantOrder) {
		t.Fatalf("made %d commands, want %d: %v", len(calls), len(wantOrder), calls)
	}
	for i, want := range wantOrder {
		if !strings.Contains(calls[i], want) {
			t.Fatalf("command %d = %q, want it to contain %q", i, calls[i], want)
		}
	}
}

func TestRunVarnishCheckStopsWhenVarnishRejectsTheVCL(t *testing.T) {
	var calls []string
	cp := &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			joined := strings.Join(args, " ")
			calls = append(calls, joined)
			if strings.Contains(joined, "vcl.load") {
				return exec.CommandContext(ctx, "sh", "-c", "echo 'Message from VCC-compiler'; exit 1")
			}
			return exec.CommandContext(ctx, "true")
		},
	}

	status, log := runVarnishCheck(context.Background(), cp)

	if status != result.StatusFail {
		t.Fatalf("status = %q, want fail", status)
	}
	if strings.Contains(strings.Join(calls, "\n"), "check-varnish.php") {
		t.Fatalf("ran the check after the VCL was rejected: %v", calls)
	}
	if got := classifyStepFailure("varnish", log); got == nil || got.Code != "varnish_vcl_rejected" {
		t.Fatalf("classification = %#v, want varnish_vcl_rejected", got)
	}
}

func TestRunVarnishCheckReportsHarnessSteps(t *testing.T) {
	fastBackendWait(t)
	for _, failing := range []string{"cp php-fpm:", "cp /", "vcl.use", "ban req.url"} {
		t.Run(failing, func(t *testing.T) {
			cp := &Compose{
				projectName: "m2test-abcd",
				files:       []string{"/tmp/base.yml"},
				execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
					joined := strings.Join(args, " ")
					if strings.Contains(joined, "backend.list") {
						return exec.CommandContext(ctx, "echo", "magento.default probe 5/5 healthy")
					}
					if strings.Contains(joined, failing) && !strings.Contains(joined, "run-check.sh") {
						return exec.CommandContext(ctx, "sh", "-c", "exit 1")
					}
					return exec.CommandContext(ctx, "true")
				},
			}

			status, log := runVarnishCheck(context.Background(), cp)

			if status != result.StatusFail {
				t.Fatalf("status = %q, want fail", status)
			}
			if got := classifyStepFailure("varnish", log); got == nil || got.Code != "check_error" {
				t.Fatalf("classification = %#v, want check_error", got)
			}
		})
	}
}

func TestRunVarnishCheckReportsAVarnishadmThatKeepsFailing(t *testing.T) {
	fastBackendWait(t)
	cp := &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if strings.Contains(strings.Join(args, " "), "backend.list") {
				return exec.CommandContext(ctx, "sh", "-c", "exit 1")
			}
			return exec.CommandContext(ctx, "true")
		},
	}

	status, log := runVarnishCheck(context.Background(), cp)

	if status != result.StatusFail {
		t.Fatalf("status = %q, want fail", status)
	}
	if got := classifyStepFailure("varnish", log); got == nil || got.Code != "check_error" {
		t.Fatalf("classification = %#v, want check_error", got)
	}
}

func TestRunVarnishCheckFailsWhenTheBackendStaysSick(t *testing.T) {
	fastBackendWait(t)
	var calls []string
	cp := &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			joined := strings.Join(args, " ")
			calls = append(calls, joined)
			if strings.Contains(joined, "backend.list") {
				return exec.CommandContext(ctx, "echo", "magento.default probe 0/5 sick")
			}
			return exec.CommandContext(ctx, "true")
		},
	}

	status, log := runVarnishCheck(context.Background(), cp)

	if status != result.StatusFail {
		t.Fatalf("status = %q, want fail", status)
	}
	if strings.Contains(strings.Join(calls, "\n"), "check-varnish.php") {
		t.Fatalf("ran the check against a sick backend: %v", calls)
	}
	if got := classifyStepFailure("varnish", log); got == nil || got.Code != "varnish_probe_failing" {
		t.Fatalf("classification = %#v, want varnish_probe_failing", got)
	}
}

func TestRunVarnishCheckClassifiesVCLLoadOutput(t *testing.T) {
	cases := []struct {
		name, output, want string
	}{
		{"syntax error", "VCL compilation failed\nMessage from VCC-compiler:\nExpected ';' got '}'", "varnish_vcl_rejected"},
		{"backend host does not resolve", "Message from VCC-compiler:\nBackend host \"webserver\": resolution error (Name or service not known)\nVCL compilation failed", "check_error"},
		{"varnishadm cannot connect", "Cannot connect to CLI port", "check_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := &Compose{
				projectName: "m2test-abcd",
				files:       []string{"/tmp/base.yml"},
				execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
					if strings.Contains(strings.Join(args, " "), "vcl.load") {
						return exec.CommandContext(ctx, "sh", "-c", "printf '%s\\n' \"$0\"; exit 1", tc.output)
					}
					return exec.CommandContext(ctx, "true")
				},
			}

			status, log := runVarnishCheck(context.Background(), cp)

			if status != result.StatusFail {
				t.Fatalf("status = %q, want fail", status)
			}
			if got := classifyStepFailure("varnish", log); got == nil || got.Code != tc.want {
				t.Fatalf("classification = %#v, want %s; log:\n%s", got, tc.want, log)
			}
		})
	}
}

func TestClassifyVarnishFailures(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want string
	}{
		{"no hit", "[FAIL] varnish: the second GET of the product page was not a cache hit", "varnish_not_caching"},
		{"fpc still built in", "[FAIL] varnish: full page cache is not set to Varnish", "varnish_not_caching"},
		{"no purge", "[FAIL] varnish: the product page was not purged when the product was saved", "varnish_not_purged"},
		{"probe failing", "[FAIL] varnish: the health probe /health_check.php answers 500, so Varnish would mark the backend sick", "varnish_probe_failing"},
		{"product page not 200", "[FAIL] varnish: the product page did not answer 200 (status 503, x-magento-cache-debug ''; status 503, x-magento-cache-debug '')", "varnish_bad_response"},
		{"page not 200 after the purge", "[FAIL] varnish: the product page did not answer 200 after the product was saved (status 500, x-magento-cache-debug '')", "varnish_bad_response"},
		{"private page cached", "[FAIL] varnish: /checkout/cart/ was served from the cache", "varnish_private_page_cached"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStepFailure("varnish", tc.log)
			if got == nil || got.Code != tc.want {
				t.Fatalf("classifyStepFailure(varnish) = %#v, want code %s", got, tc.want)
			}
		})
	}
}

func TestVarnishVCLHasNoTemplatePlaceholders(t *testing.T) {
	raw, err := os.ReadFile("../../../docker/scripts/tests/varnish.vcl")
	if err != nil {
		t.Fatal(err)
	}
	vcl := string(raw)
	for _, want := range []string{`.host = "webserver";`, `.port = "80";`, `"php-fpm";`, `.url = "/health_check.php";`} {
		if !strings.Contains(vcl, want) {
			t.Errorf("varnish.vcl lacks %s", want)
		}
	}
	if strings.Contains(vcl, "{{") {
		t.Error("varnish.vcl still has template placeholders")
	}
}
