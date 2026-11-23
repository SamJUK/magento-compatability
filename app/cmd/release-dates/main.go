// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// release-dates adds new upstream releases to matrix.yml and writes the date each release in it
// came out.
//
//	go run ./cmd/release-dates                # from app/, writes ../matrix.yml and ../site/src/data/release-dates.json
//
// New releases come from each product's Composer repository in matrix.yml, the one the harness
// installs from. A new Mage-OS release copies the baseline of the nearest version below it.
//
// Magento baselines follow Adobe's system requirements (adobe.go): a new release takes Adobe's
// values, or copies the version below it until Adobe publishes them, and existing baselines are
// realigned each run unless listed under keep_baseline.
//
// Magento dates come from magento.watch by Lukasz Bajsarowicz (https://magento.watch/api).
// Mage-OS dates come from the mage-os/mageos-magento2 GitHub releases, falling back to the
// tagged commit for tags that have no release. Set GITHUB_TOKEN to lift the API rate limit.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

const (
	magentoWatchURL = "https://magento.watch/api/v1/magento-community/versions"
	mageOSRepo      = "https://api.github.com/repos/mage-os/mageos-magento2"
)

var client = &http.Client{Timeout: 30 * time.Second}

func main() {
	matrixPath := flag.String("matrix", "../matrix.yml", "path to matrix.yml")
	out := flag.String("out", "../site/src/data/release-dates.json", "file to write")
	summary := flag.String("summary", "", "file to write a Markdown list of added releases to")
	flag.Parse()

	if err := sync(*matrixPath, *out, *summary); err != nil {
		fmt.Fprintln(os.Stderr, "release-dates:", err)
		os.Exit(1)
	}
}

// sync fetches and computes everything before writing any file, so a failed source leaves them all
// as they were. Adobe values the services list can't run stay as they were and fail the run once
// everything else is written.
func sync(matrixPath, out, summaryPath string) error {
	r, err := syncVersions(matrixPath)
	if err != nil {
		return err
	}
	m, err := matrix.Parse([]byte(r.yml))
	if err != nil {
		return fmt.Errorf("matrix.yml no longer parses after editing: %w", err)
	}
	dates, err := releaseDates(m, out)
	if err != nil {
		return err
	}
	fmt.Print(r.summary)
	files := map[string]string{matrixPath: r.yml, out: dates}
	if summaryPath != "" {
		files[summaryPath] = r.summary
	}
	if err := writeAll(files); err != nil {
		return err
	}
	if len(r.gaps) > 0 {
		return fmt.Errorf("Adobe recommends versions the services list can't run, add them to services in matrix.yml:\n%s", strings.Join(r.gaps, "\n"))
	}
	return nil
}

type synced struct {
	yml, summary string
	gaps         []string
}

func syncVersions(matrixPath string) (synced, error) {
	m, err := matrix.Load(matrixPath)
	if err != nil {
		return synced{}, err
	}
	raw, err := os.ReadFile(matrixPath)
	if err != nil {
		return synced{}, err
	}
	yml := string(raw)
	reqs, err := fetchRequirements()
	if err != nil {
		return synced{}, fmt.Errorf("fetching Adobe system requirements: %w", err)
	}

	var all []added
	for _, p := range m.Products {
		upstream, err := upstreamVersions(p)
		if err != nil {
			return synced{}, fmt.Errorf("%s releases: %w", p.Name, err)
		}
		var have []string
		for _, v := range p.Versions {
			have = append(have, v.Version)
		}
		var log []added
		if yml, log, err = addVersions(yml, p.Name, have, missingVersions(have, p.Skip, upstream)); err != nil {
			return synced{}, err
		}
		all = append(all, log...)
	}
	if m, err = matrix.Parse([]byte(yml)); err != nil {
		return synced{}, fmt.Errorf("matrix.yml no longer parses after adding releases: %w", err)
	}
	var newVersions []string
	for _, a := range all {
		if a.Product == "magento" {
			newVersions = append(newVersions, a.Version)
		}
	}
	var changes []baselineChange
	var warnings []baselineWarning
	var keep []string
	for _, p := range m.Products {
		if p.Name != "magento" {
			continue
		}
		keep = p.KeepBaseline
		if yml, changes, warnings, err = applyAdobe(yml, p.Versions, p.KeepBaseline, m.Services, reqs, newVersions); err != nil {
			return synced{}, err
		}
	}
	var gaps []string
	for _, w := range warnings {
		gaps = append(gaps, fmt.Sprintf("- magento %s %s", w.Version, w.Text))
	}

	var md strings.Builder
	for _, a := range all {
		fmt.Fprintf(&md, "- %s %s: ", a.Product, a.Version)
		if _, ok := reqs[a.Version]; ok && a.Product == "magento" && !slices.Contains(keep, a.Version) {
			md.WriteString("baseline from Adobe's system requirements")
			if hasWarning(warnings, a.Version) {
				fmt.Fprintf(&md, ", with some values copied from %s (below)", a.From)
			}
		} else {
			fmt.Fprintf(&md, "baseline copied from %s", a.From)
			if a.NewLine {
				md.WriteString(". **New line: check the baseline against the system requirements.**")
			}
		}
		md.WriteString("\n")
	}
	if len(changes) > 0 {
		if md.Len() > 0 {
			md.WriteString("\n")
		}
		md.WriteString("Baselines changed to match Adobe's system requirements:\n\n")
		for _, c := range changes {
			fmt.Fprintf(&md, "- magento %s %s: %s to %s\n", c.Version, c.Field, c.From, c.To)
		}
	}
	if len(gaps) > 0 {
		if md.Len() > 0 {
			md.WriteString("\n")
		}
		md.WriteString("**Adobe recommends versions the services list can't run.** Add them to services in matrix.yml on this PR:\n\n")
		md.WriteString(strings.Join(gaps, "\n") + "\n")
	}
	return synced{yml, md.String(), gaps}, nil
}

// writeAll stages every changed file next to its target before renaming any, so a file that
// can't be written leaves the others as they were.
func writeAll(files map[string]string) error {
	staged := map[string]string{}
	defer func() {
		for _, tmp := range staged {
			os.Remove(tmp)
		}
	}()
	for path, content := range files {
		if old, err := os.ReadFile(path); err == nil && string(old) == content {
			continue
		}
		tmp := path + ".tmp"
		staged[path] = tmp
		if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
			return err
		}
	}
	for path, tmp := range staged {
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
		delete(staged, path)
	}
	return nil
}

// releaseDates returns the new release-dates.json for the versions in m.
func releaseDates(m *matrix.Matrix, out string) (string, error) {
	magento, err := magentoDates()
	if err != nil {
		return "", fmt.Errorf("magento.watch: %w", err)
	}
	mageos, err := mageOSDates()
	if err != nil {
		return "", fmt.Errorf("mage-os GitHub: %w", err)
	}

	// A source that drops a version keeps its last known date, so a bad fetch cannot wipe the file.
	previous := map[string]map[string]string{}
	if old, err := os.ReadFile(out); err == nil {
		if err := json.Unmarshal(old, &previous); err != nil {
			return "", fmt.Errorf("%s: %w", out, err)
		}
	}

	dates := map[string]map[string]string{}
	sources := map[string]map[string]string{"magento": magento, "mageos": mageos}
	for _, p := range m.Products {
		picked, missing := pick(p.Versions, sources[p.Name], previous[p.Name])
		for _, v := range missing {
			fmt.Fprintf(os.Stderr, "warning: no release date for %s %s\n", p.Name, v)
		}
		dates[p.Name] = picked
	}

	body, err := json.MarshalIndent(dates, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body) + "\n", nil
}

// pick keeps the dates for the versions under test, so the file only changes when one of them does.
func pick(versions []matrix.ProductVersion, known, previous map[string]string) (map[string]string, []string) {
	picked := map[string]string{}
	var missing []string
	for _, v := range versions {
		if d, ok := known[v.Version]; ok {
			picked[v.Version] = d
		} else if d, ok := previous[v.Version]; ok {
			picked[v.Version] = d
		} else {
			missing = append(missing, v.Version)
		}
	}
	return picked, missing
}

func magentoDates() (map[string]string, error) {
	var body struct {
		Data map[string]struct {
			ReleaseDate string `json:"releaseDate"`
		} `json:"data"`
	}
	if err := getJSON(magentoWatchURL, &body); err != nil {
		return nil, err
	}
	dates := map[string]string{}
	for v, r := range body.Data {
		if d, ok := day(r.ReleaseDate); ok {
			dates[v] = d
		}
	}
	return dates, nil
}

func mageOSDates() (map[string]string, error) {
	var releases []struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Draft       bool   `json:"draft"`
	}
	if err := getJSON(mageOSRepo+"/releases?per_page=100", &releases); err != nil {
		return nil, err
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if err := getJSON(mageOSRepo+"/tags?per_page=100", &tags); err != nil {
		return nil, err
	}

	dates := map[string]string{}
	for _, r := range releases {
		if d, ok := day(r.PublishedAt); ok && !r.Draft {
			dates[r.TagName] = d
		}
	}
	// Early Mage-OS versions were tagged without a GitHub release.
	for _, t := range tags {
		if _, ok := dates[t.Name]; ok {
			continue
		}
		var commit struct {
			Commit struct {
				Committer struct {
					Date string `json:"date"`
				} `json:"committer"`
			} `json:"commit"`
		}
		if err := getJSON(mageOSRepo+"/commits/"+t.Name, &commit); err != nil {
			// The previous date for this tag, if any, stands in for it.
			fmt.Fprintf(os.Stderr, "warning: skipping Mage-OS tag %s: %v\n", t.Name, err)
			continue
		}
		if d, ok := day(commit.Commit.Committer.Date); ok {
			dates[t.Name] = d
		}
	}
	return dates, nil
}

// day turns "2026-05-12" or "2026-05-12T10:00:00Z" into "2026-05-12".
func day(s string) (string, bool) {
	if len(s) < 10 {
		return "", false
	}
	if _, err := time.Parse(time.DateOnly, s[:10]); err != nil {
		return "", false
	}
	return s[:10], true
}

func getJSON(url string, v any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "magento.works release-dates (+https://magento.works)")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" && strings.HasPrefix(url, mageOSRepo) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
