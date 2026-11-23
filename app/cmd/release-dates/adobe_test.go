// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

const adobeFixture = `---
# comment
2.4.10-alpha:
  PHP: '8.5'
2.4.9:
  PHP: '8.5'
  MariaDB: '12.3'
  MySQL: '8.4'
  OpenSearch: '3'
  RabbitMQ: '4.3'
  Valkey: '9'
  Varnish: '8'
2.4.8-p5 (latest):
  PHP: '8.4, 8.3'
  MariaDB: '11.4, 11.8'
  MySQL: '8.4'
  Elasticsearch: '8'
  OpenSearch: 3.7+
  RabbitMQ: '4.3'
  Valkey: '8.1'
  Varnish: '8'
  AWS S3: ✔️
2.4.4:
  PHP: '8.1'
  MariaDB: '10.4'
  MySQL: '8.0'
  Elasticsearch: '7.17/8'
  OpenSearch: '1.2'
  RabbitMQ: '3.9'
  Redis: '6.2'
  Varnish: '7.0'
`

var testServices = matrix.Services{
	PHP: []string{"8.5", "8.4", "8.3", "8.1"},
	Database: []matrix.ServiceSpec{
		{Type: "mysql", Version: "8.4.9"}, {Type: "mysql", Version: "8.0.46"},
		{Type: "mariadb", Version: "12.3"}, {Type: "mariadb", Version: "11.4"}, {Type: "mariadb", Version: "10.4"},
	},
	Search: []matrix.ServiceSpec{
		{Type: "opensearch", Version: "3"}, {Type: "opensearch", Version: "1.3.20"},
		{Type: "elasticsearch", Version: "8.19.15"}, {Type: "elasticsearch", Version: "7.16.3"},
	},
	Cache: []matrix.ServiceSpec{
		{Type: "valkey", Version: "9"}, {Type: "valkey", Version: "8"},
		{Type: "redis", Version: "7.2"}, {Type: "redis", Version: "6.2.22"},
	},
	Queue:   []matrix.ServiceSpec{{Type: "rabbitmq", Version: "4.3"}, {Type: "rabbitmq", Version: "3.9"}},
	Varnish: []string{"8.0", "7.7", "none"},
}

func TestParseRequirements(t *testing.T) {
	reqs, err := parseRequirements([]byte(adobeFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"2.4.9", "2.4.8-p5", "2.4.4"} {
		if _, ok := reqs[v]; !ok {
			t.Errorf("missing %s in %v", v, reqs)
		}
	}
	if _, ok := reqs["2.4.10-alpha"]; ok {
		t.Error("previews should be dropped")
	}
	if got := reqs["2.4.8-p5"]["OpenSearch"]; got != "3.7+" {
		t.Errorf("OpenSearch = %q, want 3.7+", got)
	}
	if _, err := parseRequirements([]byte("2.4.10-alpha:\n  PHP: '8.5'\n")); err == nil {
		t.Error("expected an error when no stable release is listed")
	}
}

func TestCandidates(t *testing.T) {
	for in, want := range map[string][]string{
		"8.4, 8.3":                 {"8.4", "8.3"},
		"7.17/8":                   {"7.17", "8"},
		"7.17,8.17":                {"7.17", "8.17"},
		"3.7+":                     {"3.7"},
		"2.54.0 (latest)":          {"2.54.0"},
		"3.13 or latest available": {"3.13"},
		"2.10":                     {"2.10"},
		"":                         nil,
		"✔️":                       nil,
	} {
		if got := candidates(in); !reflect.DeepEqual(got, want) {
			t.Errorf("candidates(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRunnable(t *testing.T) {
	available := []string{"2.19.5", "2.12.0", "8", "3"}
	for want, expect := range map[string]string{
		"2.19": "2.19.5", "3": "3", "3.7": "3", "8.1": "8", "2.19.5": "2.19.5", "1.2": "", "2": "2.19.5", "4": "",
	} {
		got, ok := runnable(want, available)
		if got != expect || ok != (expect != "") {
			t.Errorf("runnable(%q) = %q, %v; want %q", want, got, ok, expect)
		}
	}
}

func baselineOf(db, search, cache string) matrix.Baseline {
	spec := func(s string) matrix.ServiceSpec {
		typ, ver, _ := strings.Cut(s, " ")
		return matrix.ServiceSpec{Type: typ, Version: ver}
	}
	return matrix.Baseline{PHP: "8.1", Webserver: "nginx", DB: spec(db), Search: spec(search), Cache: spec(cache), Queue: spec("rabbitmq 3.9"), Varnish: "7.7"}
}

func TestBaselineFromAdobe(t *testing.T) {
	reqs, _ := parseRequirements([]byte(adobeFixture))

	got, warnings := baselineFromAdobe(baselineOf("mariadb 10.4", "opensearch 3", "redis 6.2.22"), reqs["2.4.8-p5"], testServices)
	want := baselineOf("mariadb 11.4", "opensearch 3", "valkey 8")
	want.PHP, want.Queue, want.Varnish = "8.4", matrix.ServiceSpec{Type: "rabbitmq", Version: "4.3"}, "8.0"
	if got != want {
		t.Errorf("2.4.8-p5: got %+v, want %+v", got, want)
	}
	if len(warnings) != 0 {
		t.Errorf("2.4.8-p5 warnings = %v", warnings)
	}

	// Adobe's OpenSearch 1.2 and Varnish 7.0 aren't in the services list, so a later minor of the
	// same major stands in. Redis 6.2 matches 6.2.22 by series.
	old := baselineOf("mysql 8.0.46", "opensearch 1.3.20", "redis 7.2")
	got, warnings = baselineFromAdobe(old, reqs["2.4.4"], testServices)
	if got.DB != (matrix.ServiceSpec{Type: "mysql", Version: "8.0.46"}) {
		t.Errorf("a MySQL baseline should keep its type, got %+v", got.DB)
	}
	if got.Search != old.Search || got.Varnish != "7.7" {
		t.Errorf("search and varnish should fall back to a later minor, got %+v, %q", got.Search, got.Varnish)
	}
	if got.Cache != (matrix.ServiceSpec{Type: "redis", Version: "6.2.22"}) {
		t.Errorf("cache = %+v", got.Cache)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q", warnings)
	}

	// With no 1.x, 7.x or Varnish 7 to fall back to, both keep their old values and warn.
	services := testServices
	services.Search = []matrix.ServiceSpec{{Type: "opensearch", Version: "3"}, {Type: "elasticsearch", Version: "8.19.15"}}
	services.Varnish = []string{"8.0"}
	old.Search = matrix.ServiceSpec{Type: "opensearch", Version: "3"}
	got, warnings = baselineFromAdobe(old, reqs["2.4.4"], services)
	if got.Search != old.Search || got.Varnish != old.Varnish {
		t.Errorf("search and varnish should be kept, got %+v, %q", got.Search, got.Varnish)
	}
	if len(warnings) != 2 || !strings.HasPrefix(warnings[0], "search:") || !strings.HasPrefix(warnings[1], "varnish:") {
		t.Errorf("warnings = %q, want search and varnish", warnings)
	}
}

func TestPickSpecSwitchesTypeOnlyWhenCurrentIsNotListed(t *testing.T) {
	adobe := map[string]string{"Elasticsearch": "8"}
	got, ok := pickSpec(adobe, matrix.ServiceSpec{Type: "opensearch", Version: "3"}, searchNames, testServices.Search)
	if !ok || got != (matrix.ServiceSpec{Type: "elasticsearch", Version: "8.19.15"}) {
		t.Errorf("got %+v, %v", got, ok)
	}
}

const adobeMatrix = `products:
  - name: magento
    versions:
      - version: "2.4.9"
        baseline:
          php: "8.5"
          webserver: nginx
          db: { type: mariadb, version: "12.3" }
          search: { type: opensearch, version: "3" }
          cache: { type: valkey, version: "9" }
          queue: { type: rabbitmq, version: "4.3" }
          varnish: "8.0"

      ######################
      # MAGENTO 2.4.8 LINE #
      ######################
      - version: "2.4.8-p5"
        baseline:
          php: "8.3"
          webserver: apache
          db: { type: mariadb, version: "10.4" }
          search: { type: opensearch, version: "3" }
          cache: { type: redis, version: "7.2" }
          queue: { type: rabbitmq, version: "4.3" }
          varnish: "8.0"

      - version: "2.4.8-p4"
        baseline:
          php: "8.3"
  - name: mageos
    versions:
      - version: "3.6.0"
        baseline:
          php: "8.3"

services:
  php:
    - "8.5"
`

func TestApplyAdobe(t *testing.T) {
	reqs, _ := parseRequirements([]byte(adobeFixture))
	m := loadString(t, adobeMatrix)

	yml, changes, warnings, err := applyAdobe(adobeMatrix, m.Products[0].Versions, nil, testServices, reqs, nil)
	if err != nil {
		t.Fatal(err)
	}

	wantChanges := []baselineChange{
		{"2.4.8-p5", "php", "8.3", "8.4"},
		{"2.4.8-p5", "db", "mariadb 10.4", "mariadb 11.4"},
		{"2.4.8-p5", "cache", "redis 7.2", "valkey 8"},
	}
	if !reflect.DeepEqual(changes, wantChanges) {
		t.Errorf("changes = %+v, want %+v", changes, wantChanges)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
	for _, want := range []string{
		"      # MAGENTO 2.4.8 LINE #\n",
		"          php: \"8.4\"\n          webserver: apache\n          db: { type: mariadb, version: \"11.4\" }\n",
		"          cache: { type: valkey, version: \"8\" }\n",
		"      - version: \"2.4.8-p4\"\n        baseline:\n          php: \"8.3\"\n  - name: mageos\n    versions:\n      - version: \"3.6.0\"\n        baseline:\n          php: \"8.3\"\n",
	} {
		if !strings.Contains(yml, want) {
			t.Errorf("missing %q in:\n%s", want, yml)
		}
	}
	if !strings.HasPrefix(yml, strings.Split(adobeMatrix, "      - version: \"2.4.8-p5\"")[0]) {
		t.Errorf("2.4.9 entry changed:\n%s", yml)
	}

	_, again, _, _ := applyAdobe(yml, loadString(t, yml).Products[0].Versions, nil, testServices, reqs, nil)
	if len(again) != 0 {
		t.Errorf("second run changed %+v", again)
	}
}

func TestApplyAdobeLeavesNewVersionsOutOfTheChangeList(t *testing.T) {
	reqs, _ := parseRequirements([]byte(adobeFixture))
	m := loadString(t, adobeMatrix)
	yml, changes, _, err := applyAdobe(adobeMatrix, m.Products[0].Versions, nil, testServices, reqs, []string{"2.4.8-p5"})
	if err != nil || len(changes) != 0 || !strings.Contains(yml, `php: "8.4"`) {
		t.Errorf("err = %v, changes = %+v", err, changes)
	}
}

func TestSetBaselineFieldFailsWithoutTheEntry(t *testing.T) {
	if _, err := setBaselineField(adobeMatrix, "magento", "9.9.9", "php", "8.4"); err == nil {
		t.Error("expected an error")
	}
	if _, err := setBaselineField(adobeMatrix, "mageos", "2.4.9", "php", "8.4"); err == nil {
		t.Error("expected an error for a version of another product")
	}
}

func loadString(t *testing.T, yml string) *matrix.Matrix {
	t.Helper()
	path := t.TempDir() + "/matrix.yml"
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := matrix.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestApplyAdobeSkipsKeptBaselines(t *testing.T) {
	reqs, _ := parseRequirements([]byte(adobeFixture))
	m := loadString(t, adobeMatrix)
	yml, changes, _, err := applyAdobe(adobeMatrix, m.Products[0].Versions, []string{"2.4.8-p5"}, testServices, reqs, nil)
	if err != nil || len(changes) != 0 || yml != adobeMatrix {
		t.Errorf("err = %v, changes = %+v", err, changes)
	}
}

func TestPickSpecFallsBackToTheNextListedVersion(t *testing.T) {
	adobe := map[string]string{"RabbitMQ": "4.1, 3.13"}
	queues := []matrix.ServiceSpec{{Type: "rabbitmq", Version: "4.3"}, {Type: "rabbitmq", Version: "3.13"}}
	got, ok := pickSpec(adobe, matrix.ServiceSpec{Type: "rabbitmq", Version: "4.2"}, queueNames, queues)
	if !ok || got.Version != "3.13" {
		t.Errorf("got %+v, %v", got, ok)
	}
}

func TestPickSpecKeepsACurrentVersionAdobeLists(t *testing.T) {
	adobe := map[string]string{"Elasticsearch": "7.17", "OpenSearch": "1.3"}
	current := matrix.ServiceSpec{Type: "elasticsearch", Version: "7.17.28"}
	if got, ok := pickSpec(adobe, current, searchNames, testServices.Search); !ok || got != current {
		t.Errorf("got %+v, %v", got, ok)
	}
}

func TestSetBaselineFieldKeepsComments(t *testing.T) {
	yml := "products:\n  - name: magento\n    versions:\n      - version: \"2.4.8\" # note\n        baseline:\n          php: \"8.3\" # pinned for X\n"
	got, err := setBaselineField(yml, "magento", "2.4.8", "php", "8.4")
	if err != nil || !strings.Contains(got, "php: \"8.4\" # pinned for X\n") {
		t.Errorf("err = %v, got:\n%s", err, got)
	}
}

func TestPickSpecLeavesTypesAdobeDoesNotList(t *testing.T) {
	adobe := map[string]string{"MySQL": "8.4"}
	current := matrix.ServiceSpec{Type: "percona", Version: "8.0"}
	if got, ok := pickSpec(adobe, current, dbNames, testServices.Database); !ok || got != current {
		t.Errorf("got %+v, %v", got, ok)
	}
}

func TestFirstRunnableFallsBackToALaterMinor(t *testing.T) {
	available := []string{"6.0", "7.7", "8.0"}
	if v, ok := firstRunnable([]string{"7.5"}, available); !ok || v != "7.7" {
		t.Errorf("7.5: got %q, %v, want 7.7", v, ok)
	}
	if v, ok := firstRunnable([]string{"8.0", "7.5"}, available); !ok || v != "8.0" {
		t.Errorf("an exact match should win, got %q", v)
	}
	if v, ok := firstRunnable([]string{"9.1"}, available); ok {
		t.Errorf("9.1 has no 9.x to fall back to, got %q", v)
	}
	if v, ok := firstRunnable([]string{"7.8"}, available); ok {
		t.Errorf("a lower minor isn't a fallback, got %q", v)
	}
}
