// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package runner

import (
	"context"
	"os/exec"
	"slices"
	"testing"
)

const testDigest = "mariadb@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func imagesCompose(host string, failAll bool) *Compose {
	reply := func(ctx context.Context, out string) *exec.Cmd {
		if failAll {
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "printf", "%b", out)
	}
	return &Compose{
		projectName: "m2test-abcd",
		files:       []string{"/tmp/base.yml"},
		execCommand: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			switch {
			case slices.Contains(args, "ps"):
				return reply(ctx, "c1\\nc2\\nc3\\n")
			case slices.Contains(args, "version"):
				return reply(ctx, host+"\\n")
			case slices.Contains(args, "image"):
				return reply(ctx, "sha256:i1\\tlinux/arm64\\t[\""+testDigest+"\"]\\n"+
					"sha256:i2\\tlinux/amd64\\t[]\\n"+
					"sha256:i3\\tlinux/arm64\\tnull\\n")
			default:
				return reply(ctx, "db\\tsha256:i1\\nsearch\\tsha256:i2\\nphp-fpm\\tsha256:i3\\n")
			}
		},
	}
}

func TestCaptureImages(t *testing.T) {
	got := captureImages(context.Background(), imagesCompose("linux/arm64", false))

	db := got["db"]
	if db.Platform != "linux/arm64" || db.Digest != testDigest || db.Emulated == nil || *db.Emulated {
		t.Errorf("db = %+v, want native arm64 with a digest", db)
	}
	search := got["search"]
	if search.Platform != "linux/amd64" || search.Digest != "" || search.Emulated == nil || !*search.Emulated {
		t.Errorf("search = %+v, want emulated amd64 without a digest", search)
	}
	if fpm := got["php-fpm"]; fpm.Digest != "" || fpm.Platform != "linux/arm64" {
		t.Errorf("php-fpm = %+v, want a platform and no digest", fpm)
	}
}

func TestCaptureImagesUnknownHostLeavesEmulatedOut(t *testing.T) {
	got := captureImages(context.Background(), imagesCompose("", false))

	if got["search"].Platform != "linux/amd64" || got["search"].Emulated != nil {
		t.Errorf("search = %+v, want a platform and unknown emulation", got["search"])
	}
}

func TestCaptureImagesInspectionFailure(t *testing.T) {
	if got := captureImages(context.Background(), imagesCompose("linux/arm64", true)); got != nil {
		t.Errorf("images = %+v, want none when docker fails", got)
	}
}
