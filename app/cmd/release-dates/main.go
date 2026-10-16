// release-dates writes the date each Magento and Mage-OS release in matrix.yml came out.
//
//	go run ./cmd/release-dates                # from app/, writes ../site/src/data/release-dates.json
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
	flag.Parse()

	if err := run(*matrixPath, *out); err != nil {
		fmt.Fprintln(os.Stderr, "release-dates:", err)
		os.Exit(1)
	}
}

func run(matrixPath, out string) error {
	m, err := matrix.Load(matrixPath)
	if err != nil {
		return err
	}

	magento, err := magentoDates()
	if err != nil {
		return fmt.Errorf("magento.watch: %w", err)
	}
	mageos, err := mageOSDates()
	if err != nil {
		return fmt.Errorf("mage-os GitHub: %w", err)
	}

	dates := map[string]map[string]string{}
	sources := map[string]map[string]string{"magento": magento, "mageos": mageos}
	for _, p := range m.Products {
		picked, missing := pick(p.Versions, sources[p.Name])
		for _, v := range missing {
			fmt.Fprintf(os.Stderr, "warning: no release date for %s %s\n", p.Name, v)
		}
		dates[p.Name] = picked
	}

	body, err := json.MarshalIndent(dates, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(body, '\n'), 0o644)
}

// pick keeps the dates for the versions under test, so the file only changes when one of them does.
func pick(versions []matrix.ProductVersion, known map[string]string) (map[string]string, []string) {
	picked := map[string]string{}
	var missing []string
	for _, v := range versions {
		if d, ok := known[v.Version]; ok {
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
			return nil, err
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
