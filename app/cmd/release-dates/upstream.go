// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

// Stable releases only: 2.4.8, 2.4.8-p5, 4.0.0. Alphas, betas and RCs never match.
var stableVersion = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-p(\d+))?$`)

type added struct {
	Product, Version, From string
	NewLine                bool
}

// upstreamVersions lists the stable releases a product's Composer repository serves.
func upstreamVersions(p matrix.Product) ([]string, error) {
	url := strings.TrimSuffix(p.Mirror, "/") + "/p2/" + p.Package + ".json"
	var body struct {
		Packages map[string][]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := getJSON(url, &body); err != nil {
		return nil, err
	}
	var versions []string
	for _, v := range body.Packages[p.Package] {
		if stableVersion.MatchString(v.Version) {
			versions = append(versions, v.Version)
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%s lists no stable versions of %s", url, p.Package)
	}
	return versions, nil
}

// missingVersions returns the upstream releases newer than the oldest version under test that
// the matrix lacks, oldest first, so each new entry can copy the one added before it.
func missingVersions(have, upstream []string) []string {
	oldest := ""
	for _, v := range have {
		if !stableVersion.MatchString(v) {
			continue
		}
		if oldest == "" || compareVersions(v, oldest) < 0 {
			oldest = v
		}
	}
	var missing []string
	for _, v := range upstream {
		if !slices.Contains(have, v) && !slices.Contains(missing, v) && compareVersions(v, oldest) > 0 {
			missing = append(missing, v)
		}
	}
	slices.SortFunc(missing, compareVersions)
	return missing
}

// nearestBelow is the highest version in have that is lower than v.
func nearestBelow(have []string, v string) string {
	best := ""
	for _, h := range have {
		if stableVersion.MatchString(h) && compareVersions(h, v) < 0 && (best == "" || compareVersions(h, best) > 0) {
			best = h
		}
	}
	return best
}

// sameLine is true for security patches of one Magento line, 2.4.8 and 2.4.8-p6. Any other
// step, Mage-OS 3.5.0 to 3.5.1 included, may change requirements, so it counts as a new line.
func sameLine(a, b string) bool {
	pa, pb := parseVersion(a), parseVersion(b)
	return slices.Equal(pa[:3], pb[:3])
}

// addVersions writes each missing version into matrix.yml above the entry it copies its baseline from.
func addVersions(yml string, product string, have, missing []string) (string, []added, error) {
	var log []added
	for _, v := range missing {
		from := nearestBelow(have, v)
		var err error
		if yml, err = insertEntry(yml, product, from, v); err != nil {
			return "", nil, err
		}
		have = append(have, v)
		log = append(log, added{Product: product, Version: v, From: from, NewLine: !sameLine(from, v)})
	}
	return yml, log, nil
}

// insertEntry copies the entry for version from, renamed to version to, directly above it.
// It edits text rather than re-encoding the YAML so the file's comments and layout survive.
func insertEntry(yml, product, from, to string) (string, error) {
	lines := strings.Split(yml, "\n")
	inProduct := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(line, "  - name:") {
			inProduct = strings.TrimSpace(strings.TrimPrefix(line, "  - name:")) == product
			continue
		}
		if !inProduct || (trimmed != `- version: "`+from+`"` && trimmed != "- version: "+from) {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		end := i + 1
		for end < len(lines) {
			next := lines[end]
			if strings.TrimSpace(next) == "" || len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			end++
		}
		entry := slices.Clone(lines[i:end])
		entry[0] = strings.Repeat(" ", indent) + `- version: "` + to + `"`
		entry = append(entry, "")
		return strings.Join(slices.Insert(lines, i, entry...), "\n"), nil
	}
	return "", fmt.Errorf("no %s %s entry in the matrix to copy a baseline from", product, from)
}

func parseVersion(v string) [4]int {
	var n [4]int
	m := stableVersion.FindStringSubmatch(v)
	for i := 1; m != nil && i < len(m); i++ {
		n[i-1], _ = strconv.Atoi(m[i])
	}
	return n
}

func compareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	return slices.Compare(pa[:], pb[:])
}
