// Package matrix parses matrix.yml and generates test combinations using the
// baseline-deviation strategy: one baseline run per product version, plus one
// run per non-baseline option in each service dimension.
package matrix

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ── YAML types ────────────────────────────────────────────────────────────────

// Matrix is the top-level structure of matrix.yml.
type Matrix struct {
	Products []Product `yaml:"products"`
	Services Services  `yaml:"services"`
}

// Product represents a single product entry (magento, mageos, …).
type Product struct {
	Name     string           `yaml:"name"`
	Package  string           `yaml:"package"`
	Mirror   string           `yaml:"mirror"`
	Versions []ProductVersion `yaml:"versions"`
}

// ProductVersion pairs a version string with its recommended-baseline service
// set. Versions without a Baseline are skipped with a warning.
type ProductVersion struct {
	Version  string    `yaml:"version"`
	Baseline *Baseline `yaml:"baseline"`
}

// Baseline holds the officially-recommended service set for a product version.
// One combination is generated at the full baseline, then one additional
// combination per non-baseline option in each dimension (baseline-deviation).
type Baseline struct {
	PHP       string      `yaml:"php"`
	Webserver string      `yaml:"webserver"`
	DB        ServiceSpec `yaml:"db"`
	Search    ServiceSpec `yaml:"search"`
	Cache     ServiceSpec `yaml:"cache"`
	Queue     ServiceSpec `yaml:"queue"`
	Varnish   string      `yaml:"varnish"`
}

// Services holds every service dimension.
type Services struct {
	PHP       []string      `yaml:"php"`
	Webserver []ServiceSpec `yaml:"webserver"`
	Database  []ServiceSpec `yaml:"database"`
	Search    []ServiceSpec `yaml:"search"`
	Cache     []ServiceSpec `yaml:"cache"`
	Queue     []ServiceSpec `yaml:"queue"`
	Varnish   []string      `yaml:"varnish"`
}

// ServiceSpec is a service entry that carries both a type and a version.
// In matrix.yml these are objects: { type: mariadb, version: "11.4" }.
type ServiceSpec struct {
	Type    string `yaml:"type"`
	Version string `yaml:"version"`
}

// ── Load ──────────────────────────────────────────────────────────────────────

// Load reads and parses the matrix YAML file at the given path.
func Load(path string) (*Matrix, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("matrix: reading %s: %w", path, err)
	}
	var m Matrix
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("matrix: parsing %s: %w", path, err)
	}
	return &m, nil
}

// ── Combination ───────────────────────────────────────────────────────────────

// Combination holds one fully-resolved set of service versions.
type Combination struct {
	Product string `json:"product"`
	Package string `json:"package"`
	Mirror  string `json:"mirror"`
	Version string `json:"version"`

	PHP string `json:"php"`

	WebserverType    string `json:"webserver_type"`
	WebserverVersion string `json:"webserver_version"`

	DBType    string `json:"db_type"`
	DBVersion string `json:"db_version"`

	SearchType    string `json:"search_type"`
	SearchVersion string `json:"search_version"`

	CacheType    string `json:"cache_type"`
	CacheVersion string `json:"cache_version"`

	QueueType    string `json:"queue_type"`
	QueueVersion string `json:"queue_version"`

	Varnish string `json:"varnish"`
}

// ID returns a URL-safe slug that uniquely identifies this combination.
// Format matches the bash combo_id() function and the Astro result filenames.
func (c Combination) ID() string {
	var b strings.Builder
	b.WriteString(c.Product)
	b.WriteString("-")
	// version: replace dots with nothing for the slug
	b.WriteString(strings.ReplaceAll(c.Version, ".", ""))
	b.WriteString("-php")
	b.WriteString(strings.ReplaceAll(c.PHP, ".", ""))
	b.WriteString("-")
	b.WriteString(c.DBType)
	b.WriteString(strings.ReplaceAll(c.DBVersion, ".", ""))
	b.WriteString("-")
	b.WriteString(c.SearchType)
	b.WriteString(strings.ReplaceAll(c.SearchVersion, ".", ""))
	b.WriteString("-")
	b.WriteString(c.CacheType)
	b.WriteString(strings.ReplaceAll(c.CacheVersion, ".", ""))
	b.WriteString("-")
	b.WriteString(c.QueueType)
	b.WriteString(strings.ReplaceAll(c.QueueVersion, ".", ""))
	b.WriteString("-")
	b.WriteString(c.WebserverType)
	if c.Varnish != "none" && c.Varnish != "" {
		b.WriteString("-varnish")
		b.WriteString(strings.ReplaceAll(c.Varnish, ".", ""))
	}
	return b.String()
}

// ── Filter ────────────────────────────────────────────────────────────────────

// Filter restricts combinations to matching values.
// Empty string means "no filter" (accept all values for that dimension).
type Filter struct {
	Product   string
	Version   string
	PHP       string
	Webserver string
	// DB / Search / Cache / Queue accept "type:version" strings, e.g. "mariadb:11.4"
	DB      string
	Search  string
	Cache   string
	Queue   string
	Varnish string
}

// ── BuildCombinations ─────────────────────────────────────────────────────────

// resolveBaselineWebserver looks up the full ServiceSpec for the baseline
// webserver. It prints a warning to stderr and returns (_, false) on any
// configuration error so callers can skip the version gracefully.
func resolveBaselineWebserver(m *Matrix, p Product, pv ProductVersion) (ServiceSpec, bool) {
	if pv.Baseline == nil {
		fmt.Fprintf(os.Stderr, "[WARN]  no baseline defined for %s %s — skipping\n", p.Name, pv.Version)
		return ServiceSpec{}, false
	}
	for _, ws := range m.Services.Webserver {
		if ws.Type == pv.Baseline.Webserver {
			return ws, true
		}
	}
	fmt.Fprintf(os.Stderr, "[WARN]  baseline webserver %q for %s %s not found in services — skipping\n", pv.Baseline.Webserver, p.Name, pv.Version)
	return ServiceSpec{}, false
}

// BuildCombinations generates test combinations using the baseline-deviation
// strategy. For each product version that has a Baseline defined:
//   - One combination at the full baseline.
//   - One combination per non-baseline option in each service dimension,
//     holding all other dimensions at the baseline.
//
// Versions without a Baseline are skipped with a warning to stderr.
// The optional Filter narrows results by product, version, or service values.
func BuildCombinations(m *Matrix, f Filter) []Combination {
	var out []Combination

	for _, p := range m.Products {
		if f.Product != "" && p.Name != f.Product {
			continue
		}
		for _, pv := range p.Versions {
			if f.Version != "" && pv.Version != f.Version {
				continue
			}
			blWS, ok := resolveBaselineWebserver(m, p, pv)
			if !ok {
				continue
			}
			bl := pv.Baseline

			// mk builds one fully-resolved Combination.
			mk := func(php string, ws, db, search, cache, queue ServiceSpec, varnish string) Combination {
				return Combination{
					Product:          p.Name,
					Package:          p.Package,
					Mirror:           p.Mirror,
					Version:          pv.Version,
					PHP:              php,
					WebserverType:    ws.Type,
					WebserverVersion: ws.Version,
					DBType:           db.Type,
					DBVersion:        db.Version,
					SearchType:       search.Type,
					SearchVersion:    search.Version,
					CacheType:        cache.Type,
					CacheVersion:     cache.Version,
					QueueType:        queue.Type,
					QueueVersion:     queue.Version,
					Varnish:          varnish,
				}
			}
			add := func(c Combination) {
				if passesFilter(f, c) {
					out = append(out, c)
				}
			}

			// ── Baseline combination ──────────────────────────────────────────
			add(mk(bl.PHP, blWS, bl.DB, bl.Search, bl.Cache, bl.Queue, bl.Varnish))

			// ── PHP deviations ────────────────────────────────────────────────
			for _, php := range m.Services.PHP {
				if php != bl.PHP {
					add(mk(php, blWS, bl.DB, bl.Search, bl.Cache, bl.Queue, bl.Varnish))
				}
			}
			// ── Webserver deviations ──────────────────────────────────────────
			for _, ws := range m.Services.Webserver {
				if ws.Type != bl.Webserver {
					add(mk(bl.PHP, ws, bl.DB, bl.Search, bl.Cache, bl.Queue, bl.Varnish))
				}
			}
			// ── Database deviations ───────────────────────────────────────────
			for _, db := range m.Services.Database {
				if db.Type != bl.DB.Type || db.Version != bl.DB.Version {
					add(mk(bl.PHP, blWS, db, bl.Search, bl.Cache, bl.Queue, bl.Varnish))
				}
			}
			// ── Search deviations ─────────────────────────────────────────────
			for _, search := range m.Services.Search {
				if search.Type != bl.Search.Type || search.Version != bl.Search.Version {
					add(mk(bl.PHP, blWS, bl.DB, search, bl.Cache, bl.Queue, bl.Varnish))
				}
			}
			// ── Cache deviations ──────────────────────────────────────────────
			for _, cache := range m.Services.Cache {
				if cache.Type != bl.Cache.Type || cache.Version != bl.Cache.Version {
					add(mk(bl.PHP, blWS, bl.DB, bl.Search, cache, bl.Queue, bl.Varnish))
				}
			}
			// ── Queue deviations ──────────────────────────────────────────────
			for _, queue := range m.Services.Queue {
				if queue.Type != bl.Queue.Type || queue.Version != bl.Queue.Version {
					add(mk(bl.PHP, blWS, bl.DB, bl.Search, bl.Cache, queue, bl.Varnish))
				}
			}
			// ── Varnish deviations ────────────────────────────────────────────
			for _, varnish := range m.Services.Varnish {
				if varnish != bl.Varnish {
					add(mk(bl.PHP, blWS, bl.DB, bl.Search, bl.Cache, bl.Queue, varnish))
				}
			}
		}
	}

	return out
}

// BuildBaselineCombinations returns exactly one combination per product version:
// the full baseline. Useful for quickly verifying that every product version
// installs cleanly with its recommended service set.
func BuildBaselineCombinations(m *Matrix, f Filter) []Combination {
	var out []Combination

	for _, p := range m.Products {
		if f.Product != "" && p.Name != f.Product {
			continue
		}
		for _, pv := range p.Versions {
			if f.Version != "" && pv.Version != f.Version {
				continue
			}
			blWS, ok := resolveBaselineWebserver(m, p, pv)
			if !ok {
				continue
			}
			bl := pv.Baseline
			c := Combination{
				Product:          p.Name,
				Package:          p.Package,
				Mirror:           p.Mirror,
				Version:          pv.Version,
				PHP:              bl.PHP,
				WebserverType:    blWS.Type,
				WebserverVersion: blWS.Version,
				DBType:           bl.DB.Type,
				DBVersion:        bl.DB.Version,
				SearchType:       bl.Search.Type,
				SearchVersion:    bl.Search.Version,
				CacheType:        bl.Cache.Type,
				CacheVersion:     bl.Cache.Version,
				QueueType:        bl.Queue.Type,
				QueueVersion:     bl.Queue.Version,
				Varnish:          bl.Varnish,
			}
			if passesFilter(f, c) {
				out = append(out, c)
			}
		}
	}

	return out
}

// passesFilter reports whether c satisfies all non-empty fields of f.
func passesFilter(f Filter, c Combination) bool {
	if f.Product != "" && c.Product != f.Product {
		return false
	}
	if f.Version != "" && c.Version != f.Version {
		return false
	}
	if f.PHP != "" && c.PHP != f.PHP {
		return false
	}
	if f.Webserver != "" && c.WebserverType != f.Webserver {
		return false
	}
	if f.DB != "" && c.DBType+":"+c.DBVersion != f.DB {
		return false
	}
	if f.Search != "" && c.SearchType+":"+c.SearchVersion != f.Search {
		return false
	}
	if f.Cache != "" && c.CacheType+":"+c.CacheVersion != f.Cache {
		return false
	}
	if f.Queue != "" && c.QueueType+":"+c.QueueVersion != f.Queue {
		return false
	}
	if f.Varnish != "" && c.Varnish != f.Varnish {
		return false
	}
	return true
}
