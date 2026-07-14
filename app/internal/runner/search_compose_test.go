package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSearchComposeDisablesDiskWatermarkThresholds(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	paths := []string{
		filepath.Join(repoRoot, "docker", "compose", "services", "search-elasticsearch.yml"),
		filepath.Join(repoRoot, "docker", "compose", "services", "search-opensearch.yml"),
	}

	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(content), `cluster.routing.allocation.disk.threshold_enabled: "false"`) {
			t.Fatalf("%s: missing disk threshold override", path)
		}
	}
}
