// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestStableVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"2.4.8": true, "2.4.8-p5": true, "2.4.10": true, "4.0.0": true,
		"2.4.10-alpha1": false, "2.4.9-beta1": false, "2.0.0-rc2": false, "dev-main": false,
		"2.4.8-p5\n": false, `2.4.8"`: false,
	} {
		if got := stableVersion.MatchString(v); got != want {
			t.Errorf("stableVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{"2.4.8", "2.4.8-p1", "2.4.8-p10", "2.4.9", "2.4.10", "3.0.0"}
	for i := 1; i < len(ordered); i++ {
		if compareVersions(ordered[i-1], ordered[i]) >= 0 {
			t.Errorf("%s should sort before %s", ordered[i-1], ordered[i])
		}
	}
}

func TestMissingVersions(t *testing.T) {
	have := []string{"2.4.2", "2.4.8-p5", "2.4.9"}
	upstream := []string{"2.4.0", "2.4.1", "2.4.2", "2.4.10", "2.4.8-p6", "2.4.9", "2.4.4-p14"}

	got := missingVersions(have, upstream)

	want := []string{"2.4.4-p14", "2.4.8-p6", "2.4.10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missingVersions = %v, want %v", got, want)
	}
}

const testMatrix = `products:
  - name: magento
    versions:
      - version: "2.4.9"
        baseline:
          php: "8.5"

      ######################
      # MAGENTO 2.4.8 LINE #
      ######################
      - version: "2.4.8-p5"
        baseline:
          php: "8.4"
          varnish: "8.0"

      - version: "2.4.8"
        baseline:
          php: "8.3"
  - name: mageos
    versions:
      - version: "3.6.0"
        baseline:
          php: "8.5"

services:
  php:
    - "8.5"
`

func TestAddVersions(t *testing.T) {
	yml, log, err := addVersions(testMatrix, "magento",
		[]string{"2.4.9", "2.4.8-p5", "2.4.8"}, []string{"2.4.8-p6", "2.4.8-p7", "2.4.10"})
	if err != nil {
		t.Fatal(err)
	}

	want := `      - version: "2.4.10"
        baseline:
          php: "8.5"

      - version: "2.4.9"
        baseline:
          php: "8.5"

      ######################
      # MAGENTO 2.4.8 LINE #
      ######################
      - version: "2.4.8-p7"
        baseline:
          php: "8.4"
          varnish: "8.0"

      - version: "2.4.8-p6"
        baseline:
          php: "8.4"
          varnish: "8.0"

      - version: "2.4.8-p5"`
	if !strings.Contains(yml, want) {
		t.Errorf("matrix after adding:\n%s", yml)
	}
	if !strings.Contains(yml, "  - name: mageos\n    versions:\n      - version: \"3.6.0\"") {
		t.Errorf("mageos section changed:\n%s", yml)
	}

	wantLog := []added{
		{"magento", "2.4.8-p6", "2.4.8-p5", false},
		{"magento", "2.4.8-p7", "2.4.8-p6", false},
		{"magento", "2.4.10", "2.4.9", true},
	}
	if !reflect.DeepEqual(log, wantLog) {
		t.Errorf("log = %+v, want %+v", log, wantLog)
	}
}

func TestAddVersionsStaysInsideItsProduct(t *testing.T) {
	yml, _, err := addVersions(testMatrix, "mageos", []string{"3.6.0"}, []string{"4.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yml, "  - name: mageos\n    versions:\n      - version: \"4.0.0\"\n        baseline:\n          php: \"8.5\"\n\n      - version: \"3.6.0\"") {
		t.Errorf("matrix after adding:\n%s", yml)
	}
}

func TestAddVersionsFailsWithoutAnEntryToCopy(t *testing.T) {
	if _, _, err := addVersions(testMatrix, "mageos", []string{"9.9.9"}, []string{"10.0.0"}); err == nil {
		t.Error("expected an error")
	}
}
