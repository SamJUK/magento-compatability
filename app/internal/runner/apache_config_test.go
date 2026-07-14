package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestApacheMagentoConfigAllowsStaticJSON(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	configPath := filepath.Join(repoRoot, "docker", "apache", "magento.conf")

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read apache config: %v", err)
	}

	config := string(content)

	if strings.Contains(config, `<FilesMatch "\.(json|lock|xml)$">`) {
		t.Fatal("apache config still blocks all json files, which breaks static js-translation.json")
	}

	for _, want := range []string{
		`<LocationMatch "^/errors/.*\.(xml|phtml)$">`,
		`<LocationMatch "^/(LICENSE|composer\.(json|lock))$">`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("apache config missing expected safeguard %q", want)
		}
	}
}
