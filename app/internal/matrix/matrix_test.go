package matrix_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

// repoRoot resolves the repository root relative to this test file.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file is .../app/internal/matrix/matrix_test.go; root is three levels up
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func TestLoad(t *testing.T) {
	m, err := matrix.Load(filepath.Join(repoRoot(t), "matrix.yml"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(m.Products) == 0 {
		t.Fatal("expected at least one product")
	}
	if len(m.Services.PHP) == 0 {
		t.Fatal("expected at least one PHP version")
	}
}

func TestBuildCombinations_NoFilter(t *testing.T) {
	m, err := matrix.Load(filepath.Join(repoRoot(t), "matrix.yml"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	combos := matrix.BuildCombinations(m, matrix.Filter{})
	if len(combos) == 0 {
		t.Fatal("expected non-empty combination list")
	}

	// Count must be at least baseline + one deviation per dimension per version.
	minExpected := countMinExpected(m)
	if len(combos) < minExpected {
		t.Errorf("expected at least %d combinations, got %d", minExpected, len(combos))
	}
}

func TestBuildCombinations_ProductFilter(t *testing.T) {
	m, err := matrix.Load(filepath.Join(repoRoot(t), "matrix.yml"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if len(m.Products) == 0 {
		t.Skip("no products in matrix")
	}
	target := m.Products[0].Name
	combos := matrix.BuildCombinations(m, matrix.Filter{Product: target})
	for _, c := range combos {
		if c.Product != target {
			t.Errorf("expected product %q, got %q", target, c.Product)
		}
	}
}

func TestCombinationID_Deterministic(t *testing.T) {
	m, err := matrix.Load(filepath.Join(repoRoot(t), "matrix.yml"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	combos := matrix.BuildCombinations(m, matrix.Filter{})
	seen := make(map[string]struct{})
	for _, c := range combos {
		id := c.ID()
		if id == "" {
			t.Error("empty ID for combination")
		}
		if _, dup := seen[id]; dup {
			t.Errorf("duplicate ID: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestBuildCombinations_BaselineFirst(t *testing.T) {
	m, err := matrix.Load(filepath.Join(repoRoot(t), "matrix.yml"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	for _, p := range m.Products {
		for _, pv := range p.Versions {
			if pv.Baseline == nil {
				continue
			}
			bl := pv.Baseline
			combos := matrix.BuildCombinations(m, matrix.Filter{
				Product: p.Name,
				Version: pv.Version,
			})
			if len(combos) == 0 {
				t.Errorf("%s %s: expected at least one combination", p.Name, pv.Version)
				continue
			}
			// First combo must be the full baseline.
			first := combos[0]
			if first.PHP != bl.PHP {
				t.Errorf("%s %s: baseline[0].PHP = %q, want %q", p.Name, pv.Version, first.PHP, bl.PHP)
			}
			if first.DBType != bl.DB.Type || first.DBVersion != bl.DB.Version {
				t.Errorf("%s %s: baseline[0].DB = %s:%s, want %s:%s",
					p.Name, pv.Version, first.DBType, first.DBVersion, bl.DB.Type, bl.DB.Version)
			}
		}
	}
}

// countMinExpected returns the minimum combination count: at least one
// (the baseline) per product version that has a baseline defined.
// Actual counts are higher because each non-baseline option in every
// service dimension adds one more combination.
func countMinExpected(m *matrix.Matrix) int {
	total := 0
	for _, p := range m.Products {
		for _, pv := range p.Versions {
			if pv.Baseline != nil {
				total++
			}
		}
	}
	return total
}
