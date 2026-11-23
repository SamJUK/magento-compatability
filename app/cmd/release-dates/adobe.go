// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

const adobeRequirementsURL = "https://raw.githubusercontent.com/AdobeDocs/commerce-operations.en/main/_jekyll/_data/system-requirements.yml"

// requirements maps a release, "2.4.8-p5", to Adobe's value for each service, "PHP" to "8.4, 8.3".
type requirements map[string]map[string]string

type baselineChange struct {
	Version, Field, From, To string
}

type baselineWarning struct {
	Version, Text string
}

func hasWarning(warnings []baselineWarning, version string) bool {
	return slices.ContainsFunc(warnings, func(w baselineWarning) bool { return w.Version == version })
}

// leadingVersion picks the number out of values such as "3.7+", "2.54.0 (latest)" or "8.0 or latest available".
var leadingVersion = regexp.MustCompile(`^\d+(?:\.\d+)*`)

func fetchRequirements() (requirements, error) {
	resp, err := client.Get(adobeRequirementsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", adobeRequirementsURL, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseRequirements(body)
}

func parseRequirements(body []byte) (requirements, error) {
	// Nodes keep the text as written, so an unquoted 2.10 is not turned into the float 2.1.
	var raw map[string]map[string]yaml.Node
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	reqs := requirements{}
	for key, services := range raw {
		// Adobe marks the newest patch of a line "2.4.8-p5 (latest)" and previews "2.4.10-alpha".
		version := strings.TrimSuffix(key, " (latest)")
		if !stableVersion.MatchString(version) {
			continue
		}
		reqs[version] = map[string]string{}
		for name, node := range services {
			if node.Kind == yaml.ScalarNode {
				reqs[version][name] = node.Value
			}
		}
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("no stable releases in Adobe's system requirements")
	}
	return reqs, nil
}

// candidates splits Adobe's value into its versions, recommended first: "8.4, 8.3", "7.17/8", "3.7+".
func candidates(value string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '/' }) {
		if v := leadingVersion.FindString(strings.TrimSpace(part)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// runnable finds the version in available that the harness can run for Adobe's version. Adobe
// names a series ("2.19") where the services list a release ("2.19.5"), and the other way round
// for floating tags ("8.1" against "8"). The first match in the services list wins, and that list
// is newest first.
func runnable(want string, available []string) (string, bool) {
	if slices.Contains(available, want) {
		return want, true
	}
	for _, a := range available {
		if strings.HasPrefix(a, want+".") {
			return a, true
		}
	}
	for _, a := range available {
		if strings.HasPrefix(want, a+".") {
			return a, true
		}
	}
	return "", false
}

func versionsOf(specs []matrix.ServiceSpec, typ string) []string {
	var out []string
	for _, s := range specs {
		if s.Type == typ {
			out = append(out, s.Version)
		}
	}
	return out
}

// firstRunnable takes the first of Adobe's versions, recommended first, that the harness can run.
func firstRunnable(listed, available []string) (string, bool) {
	for _, want := range listed {
		if v, ok := runnable(want, available); ok {
			return v, true
		}
	}
	// No exact match: a later minor of the same major is close enough (Varnish 7.7 for 7.5).
	for _, want := range listed {
		if v, ok := laterMinor(want, available); ok {
			return v, true
		}
	}
	return "", false
}

// laterMinor returns the lowest available version above want with the same major.
func laterMinor(want string, available []string) (string, bool) {
	best := ""
	for _, a := range available {
		if major(a) == major(want) && compareParts(a, want) > 0 && (best == "" || compareParts(a, best) < 0) {
			best = a
		}
	}
	return best, best != ""
}

func major(v string) string {
	m, _, _ := strings.Cut(v, ".")
	return m
}

func compareParts(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// pickSpec takes Adobe's recommended version of the current type if Adobe lists it for this
// release, so a MariaDB baseline never flips to MySQL just because Adobe lists both. It tries
// the other types only when the current one is not listed.
func pickSpec(adobe map[string]string, current matrix.ServiceSpec, names map[string]string, specs []matrix.ServiceSpec) (matrix.ServiceSpec, bool) {
	if names[current.Type] == "" {
		// Adobe doesn't list this type (Percona, Artemis), so there is nothing to align with.
		return current, true
	}
	types := []string{current.Type}
	for _, s := range specs {
		if !slices.Contains(types, s.Type) {
			types = append(types, s.Type)
		}
	}
	for _, t := range types {
		listed := candidates(adobe[names[t]])
		if len(listed) == 0 {
			continue
		}
		available := versionsOf(specs, t)
		if t == current.Type {
			// A current version that already matches Adobe stays, even if the services list lacks it.
			available = append(available, current.Version)
		}
		if v, ok := firstRunnable(listed, available); ok {
			return matrix.ServiceSpec{Type: t, Version: v}, true
		}
		return current, false
	}
	return current, false
}

var (
	dbNames     = map[string]string{"mariadb": "MariaDB", "mysql": "MySQL"}
	searchNames = map[string]string{"opensearch": "OpenSearch", "elasticsearch": "Elasticsearch"}
	cacheNames  = map[string]string{"valkey": "Valkey", "redis": "Redis"}
	queueNames  = map[string]string{"rabbitmq": "RabbitMQ"}
)

// baselineFromAdobe returns b with Adobe's recommendations applied, and a warning for each
// service where they cannot be run by the harness.
func baselineFromAdobe(b matrix.Baseline, adobe map[string]string, s matrix.Services) (matrix.Baseline, []string) {
	var warnings []string
	spec := func(field string, cur *matrix.ServiceSpec, names map[string]string, available []matrix.ServiceSpec) {
		got, ok := pickSpec(adobe, *cur, names, available)
		if !ok {
			var listed []string
			for _, name := range names {
				if adobe[name] != "" {
					listed = append(listed, name+" "+adobe[name])
				}
			}
			slices.Sort(listed)
			warnings = append(warnings, fmt.Sprintf("%s: Adobe lists %q, which the services list cannot run", field, strings.Join(listed, "; ")))
			return
		}
		*cur = got
	}

	if i := slices.IndexFunc(candidates(adobe["PHP"]), func(v string) bool { return slices.Contains(s.PHP, v) }); i >= 0 {
		b.PHP = candidates(adobe["PHP"])[i]
	} else {
		warnings = append(warnings, fmt.Sprintf("php: Adobe lists %q, which the services list cannot run", adobe["PHP"]))
	}
	spec("db", &b.DB, dbNames, s.Database)
	spec("search", &b.Search, searchNames, s.Search)
	spec("cache", &b.Cache, cacheNames, s.Cache)
	spec("queue", &b.Queue, queueNames, s.Queue)
	if c := candidates(adobe["Varnish"]); len(c) > 0 {
		if v, ok := firstRunnable(c, s.Varnish); ok {
			b.Varnish = v
		} else {
			warnings = append(warnings, fmt.Sprintf("varnish: Adobe lists %q, which the services list cannot run", adobe["Varnish"]))
		}
	} else {
		warnings = append(warnings, "varnish: Adobe lists no version")
	}
	return b, warnings
}

// applyAdobe rewrites the baseline of every Magento version Adobe lists, except those in keep. A new
// version copied from a lower one takes Adobe's values here too, and is left out of the change list.
func applyAdobe(yml string, versions []matrix.ProductVersion, keep []string, services matrix.Services, reqs requirements, newVersions []string) (string, []baselineChange, []baselineWarning, error) {
	var changes []baselineChange
	var warnings []baselineWarning
	for _, v := range versions {
		adobe, ok := reqs[v.Version]
		if !ok || v.Baseline == nil || slices.Contains(keep, v.Version) {
			continue
		}
		want, warns := baselineFromAdobe(*v.Baseline, adobe, services)
		for _, w := range warns {
			warnings = append(warnings, baselineWarning{v.Version, w})
		}
		for _, c := range diffBaseline(*v.Baseline, want) {
			var err error
			if yml, err = setBaselineField(yml, "magento", v.Version, c.Field, c.To); err != nil {
				return "", nil, nil, err
			}
			if !slices.Contains(newVersions, v.Version) {
				c.Version = v.Version
				changes = append(changes, c)
			}
		}
	}
	return yml, changes, warnings, nil
}

func specText(s matrix.ServiceSpec) string { return s.Type + " " + s.Version }

func diffBaseline(old, want matrix.Baseline) []baselineChange {
	var out []baselineChange
	add := func(field, from, to string) {
		if from != to {
			out = append(out, baselineChange{Field: field, From: from, To: to})
		}
	}
	add("php", old.PHP, want.PHP)
	add("db", specText(old.DB), specText(want.DB))
	add("search", specText(old.Search), specText(want.Search))
	add("cache", specText(old.Cache), specText(want.Cache))
	add("queue", specText(old.Queue), specText(want.Queue))
	add("varnish", old.Varnish, want.Varnish)
	return out
}

// isVersionLine reports whether a trimmed matrix.yml line starts the entry for version.
func isVersionLine(trimmed, version string) bool {
	v, _, _ := strings.Cut(trimmed, " #")
	v = strings.TrimSpace(v)
	return v == `- version: "`+version+`"` || v == "- version: "+version
}

// setBaselineField replaces one baseline line of a version's entry. value is "8.4", or "type version"
// for the service blocks, which keep the inline { type: x, version: "y" } layout. Like insertEntry
// it edits text so comments survive.
func setBaselineField(yml, product, version, field, value string) (string, error) {
	lines := strings.Split(yml, "\n")
	inProduct, inEntry := false, false
	entryIndent := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if strings.HasPrefix(line, "  - name:") {
			inProduct = strings.TrimSpace(strings.TrimPrefix(line, "  - name:")) == product
			inEntry = false
			continue
		}
		if !inProduct {
			continue
		}
		if strings.HasPrefix(trimmed, "- version:") {
			inEntry = isVersionLine(trimmed, version)
			entryIndent = indent
			continue
		}
		if !inEntry || trimmed == "" || indent <= entryIndent || !strings.HasPrefix(trimmed, field+":") {
			continue
		}
		pad := strings.Repeat(" ", indent)
		if typ, ver, ok := strings.Cut(value, " "); ok {
			lines[i] = fmt.Sprintf(`%s%s: { type: %s, version: "%s" }`, pad, field, typ, ver)
		} else {
			lines[i] = fmt.Sprintf(`%s%s: "%s"`, pad, field, value)
		}
		if _, comment, ok := strings.Cut(line, " #"); ok {
			lines[i] += " #" + comment
		}
		return strings.Join(lines, "\n"), nil
	}
	return "", fmt.Errorf("no %s line in the %s %s entry", field, product, version)
}
