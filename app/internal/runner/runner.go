// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package runner orchestrates the multi-step Docker Compose test pipeline for
// a single Combination.  It is the Go equivalent of run_combination() in
// orchestrate.sh.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
)

const defaultMaxLogBytes int64 = 1 << 20 // 1 MiB per service

const (
	installRetryAttempts = 3
	installRetryDelay    = 5 * time.Second

	// Bounds on teardown and log capture, so a wedged Docker daemon can't
	// hold a slot forever.
	CleanupTimeout    = 5 * time.Minute
	diagnosticTimeout = 2 * time.Minute
)

// ErrCleanup means the stack could not be torn down, in time or at all,
// and may still be holding ports, memory or disk.
var ErrCleanup = errors.New("stack teardown failed")

var transientComposerCurlErrors = []string{
	"curl error 6",
	"curl error 7",
	"curl error 28",
	"curl error 55",
	"curl error 56",
}

var retryableInstallFailureSignatures = []string{
	"could not connect to the amqp server",
	"no alive nodes found in your cluster",
	"corrupted zip archive (0 bytes), try again",
	"does not contain valid json",
}

// FailureCodes maps every failure code Run can record to its category, so
// anything reading results can reject codes the harness never produces.
var FailureCodes = map[string]string{
	"check_error":                       "harness",
	"check_exception":                   "compatibility",
	"compile_class_missing":             "compatibility",
	"composer_allow_plugins":            "harness",
	"composer_audit_policy":             "harness",
	"composer_cache_corruption":         "harness",
	"composer_dependency_conflict":      "harness",
	"composer_network":                  "infrastructure",
	"cron_jobs_failed":                  "compatibility",
	"db_version_unsupported":            "compatibility",
	"deadline_exceeded":                 "infrastructure",
	"disk_space":                        "infrastructure",
	"docker_cleanup_race":               "harness",
	"docker_name_conflict":              "harness",
	"elasticsearch8_module_unavailable": "harness",
	"elasticsearch8_unsupported":        "compatibility",
	"glob_brace_unsupported":            "harness",
	"indexer_not_updated":               "compatibility",
	"legacy_search_flags":               "harness",
	"order_success_redirect":            "harness",
	"package_extract_timeout":           "infrastructure",
	"package_layout_invalid":            "compatibility",
	"pagebuilder_render_lock":           "harness",
	"php84_nullable_deprecation":        "compatibility",
	"php_syntax_incompatible":           "compatibility",
	"php_version_unsupported":           "compatibility",
	"playwright_browser_missing":        "harness",
	"queue_not_consumed":                "compatibility",
	"queue_not_ready":                   "harness",
	"queue_publish_failed":              "compatibility",
	"search_cluster_unhealthy":          "harness",
	"search_engine_unsupported":         "compatibility",
	"search_mapping_types_removed":      "compatibility",
	"search_not_ready":                  "harness",
	"search_results_missing":            "compatibility",
	"service_startup":                   "harness",
	"service_unhealthy":                 "harness",
	"stack_up_no_output":                "harness",
	"stomp_unsupported":                 "compatibility",
	"storefront_timeout":                "infrastructure",
	"varnish_bad_response":              "compatibility",
	"varnish_not_caching":               "compatibility",
	"varnish_not_purged":                "compatibility",
	"varnish_private_page_cached":       "compatibility",
	"varnish_probe_failing":             "compatibility",
	"varnish_vcl_rejected":              "compatibility",
}

func classifyStepFailure(stepName, log string) *result.Failure {
	return classifyStepFailureForCombination(matrix.Combination{}, stepName, log)
}

func classifyStepFailureForCombination(c matrix.Combination, stepName, log string) *result.Failure {
	text := compactWhitespace(strings.ToLower(log))

	switch {
	case strings.Contains(text, "[error] "+stepName+":"):
		return &result.Failure{
			Category:    "harness",
			Code:        "check_error",
			Summary:     "The check could not run, so it says nothing about this release.",
			LikelyFlaky: true,
		}
	case stepName == "install" && isTransientComposerNetworkFailure(text):
		return &result.Failure{
			Category:    "infrastructure",
			Code:        "composer_network",
			Summary:     "Composer download failed with a transient curl/network error.",
			LikelyFlaky: true,
		}
	case stepName == "install" && strings.Contains(text, "could not connect to the amqp server"):
		return &result.Failure{
			Category:    "harness",
			Code:        "queue_not_ready",
			Summary:     "The queue service was reachable but not yet ready when Magento validated AMQP connectivity.",
			LikelyFlaky: true,
		}
	case stepName == "install" && strings.Contains(text, "search engine cluster not healthy"):
		return &result.Failure{
			Category:    "harness",
			Code:        "search_cluster_unhealthy",
			Summary:     "The search service never reached cluster health before the harness timeout elapsed.",
			LikelyFlaky: true,
		}
	case stepName == "install" && strings.Contains(text, "/usr/bin/unzip") &&
		strings.Contains(text, "exceeded the timeout of 300 seconds"):
		return &result.Failure{
			Category:    "infrastructure",
			Code:        "package_extract_timeout",
			Summary:     "Composer package extraction exceeded the archive unzip timeout.",
			LikelyFlaky: true,
		}
	case stepName == "install" && strings.Contains(text, "requires magento/module-elasticsearch-8") &&
		strings.Contains(text, "could not be resolved"):
		return &result.Failure{
			Category:    "harness",
			Code:        "elasticsearch8_module_unavailable",
			Summary:     "The public mirrors only carry magento/module-elasticsearch-8 builds for the 8.15+ Elasticsearch client, which this release's core pins out; the matching build is only on repo.magento.com.",
			LikelyFlaky: false,
		}
	case stepName == "install" && isKnownElasticsearch8CompatibilityFailure(c, text):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "elasticsearch8_unsupported",
			Summary:     "This product version could not complete setup:install against Elasticsearch 8.x.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "search engine 'elasticsearch") &&
		strings.Contains(text, "is not an available search engine"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "search_engine_unsupported",
			Summary:     "This product version does not support the requested search engine identifier.",
			LikelyFlaky: false,
		}
	case (stepName == "install" || stepName == "smoke") && strings.Contains(text, "current version of rdbms is not supported"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "db_version_unsupported",
			Summary:     "The product version does not support the selected database version.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "your requirements could not be resolved to an installable set of packages") &&
		strings.Contains(text, "sebastian/comparator"):
		return &result.Failure{
			Category:    "harness",
			Code:        "composer_dependency_conflict",
			Summary:     "Unpinned Composer dependencies resolved to a set that conflicts with the harness root requirements.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "your php version") &&
		strings.Contains(text, "does not satisfy that requirement"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "php_version_unsupported",
			Summary:     "The product's Composer constraints do not allow this PHP version.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "glob_brace"):
		return &result.Failure{
			Category:    "harness",
			Code:        "glob_brace_unsupported",
			Summary:     "GLOB_BRACE is undefined on Alpine/musl PHP < 8.5; the musl-glob-brace patch did not apply.",
			LikelyFlaky: false,
		}
	case (stepName == "install" || stepName == "smoke") && (strings.Contains(text, "could not scan for classes inside") &&
		strings.Contains(text, "does not appear to be a file nor a folder") ||
		strings.Contains(text, "failed to open stream: no such file or directory") ||
		strings.Contains(text, "file doesn't exist")):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "package_layout_invalid",
			Summary:     "Installed package contents were incomplete or inconsistent for this release.",
			LikelyFlaky: false,
		}
	case stepName == "install" && (strings.Contains(text, "the \"--opensearch-host\" option does not exist") ||
		strings.Contains(text, "the \"--opensearch-port\" option does not exist")):
		return &result.Failure{
			Category:    "harness",
			Code:        "legacy_search_flags",
			Summary:     "The harness invoked setup:install with OpenSearch host flags that this Magento version does not support.",
			LikelyFlaky: false,
		}
	case stepName == "install" && (strings.Contains(text, "the \"--stomp-host\" option does not exist") ||
		strings.Contains(text, "the \"--queue-default-connection\" option does not exist")):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "stomp_unsupported",
			Summary:     "This release has no STOMP support (Magento_Stomp), so it cannot use ActiveMQ Artemis.",
			LikelyFlaky: false,
		}
	case stepName == "smoke" && strings.Contains(text, "no handler found for uri") && strings.Contains(text, "/_bulk"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "search_mapping_types_removed",
			Summary:     "This release indexes through mapping-type endpoints that the search engine has removed, so installs succeed but saving a product fails.",
			LikelyFlaky: false,
		}
	case stepName == "install" && strings.Contains(text, "no alive nodes found in your cluster"):
		return &result.Failure{
			Category:    "harness",
			Code:        "search_not_ready",
			Summary:     "The search service was reachable but not yet ready when Magento validated the search backend.",
			LikelyFlaky: true,
		}
	case stepName == "install" && (strings.Contains(text, "corrupted zip archive (0 bytes)") ||
		strings.Contains(text, "does not contain valid json")):
		return &result.Failure{
			Category:    "harness",
			Code:        "composer_cache_corruption",
			Summary:     "Shared Composer cache state became corrupted during the harness run.",
			LikelyFlaky: true,
		}
	case strings.Contains(text, "no space left on device"):
		return &result.Failure{
			Category:    "infrastructure",
			Code:        "disk_space",
			Summary:     "The run exhausted host or Docker disk space.",
			LikelyFlaky: true,
		}
	case stepName == "queue" && strings.Contains(text, "amq229119"):
		return &result.Failure{
			Category:    "infrastructure",
			Code:        "disk_space",
			Summary:     "Artemis stopped accepting messages because the host or Docker disk is nearly full.",
			LikelyFlaky: true,
		}
	case strings.Contains(text, "[fail] "+stepName+": uncaught "):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "check_exception",
			Summary:     "The check script stopped on an uncaught error.",
			LikelyFlaky: false,
		}
	case stepName == "queue" && strings.Contains(text, "schedulebulk returned false"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "queue_publish_failed",
			Summary:     "Magento could not publish a message to the queue service it connected to at install.",
			LikelyFlaky: false,
		}
	case stepName == "queue" && strings.Contains(text, "[fail] queue:"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "queue_not_consumed",
			Summary:     "A message was published but the consumer did not deliver it, so the queue connects but does not work.",
			LikelyFlaky: false,
		}
	case stepName == "cron" && strings.Contains(text, "cron job indexer_update_all_views ended as"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "cron_jobs_failed",
			Summary:     "Cron ran but recorded failed or missed jobs in cron_schedule.",
			LikelyFlaky: false,
		}
	case stepName == "cron" && strings.Contains(text, "[fail] cron:"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "indexer_not_updated",
			Summary:     "A product change made under update-by-schedule indexing did not reach the indexes after cron ran, or the database triggers never recorded it.",
			LikelyFlaky: false,
		}
	case stepName == "varnish" && strings.Contains(text, "rejected the vcl fixture"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "varnish_vcl_rejected",
			Summary:     "Varnish would not load the VCL fixture on this release of Magento.",
			LikelyFlaky: false,
		}
	case stepName == "varnish" && strings.Contains(text, "would mark the backend sick"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "varnish_probe_failing",
			Summary:     "The health probe in the VCL Magento generated does not answer 200, so Varnish would mark the backend sick and serve 503.",
			LikelyFlaky: false,
		}
	case stepName == "varnish" && strings.Contains(text, "did not answer 200"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "varnish_bad_response",
			Summary:     "A page the check requested through Varnish did not answer 200, so caching could not be judged.",
			LikelyFlaky: false,
		}
	case stepName == "varnish" && strings.Contains(text, "was served from the cache"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "varnish_private_page_cached",
			Summary:     "Varnish cached a page that must be passed through, such as the cart or login page.",
			LikelyFlaky: false,
		}
	case stepName == "varnish" && (strings.Contains(text, "not purged") || strings.Contains(text, "not cached again")):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "varnish_not_purged",
			Summary:     "Saving a product did not purge its page from Varnish, so customers would see stale pages.",
			LikelyFlaky: false,
		}
	case stepName == "varnish" && strings.Contains(text, "[fail] varnish:"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "varnish_not_caching",
			Summary:     "Varnish sits in front of Magento but did not cache a cacheable page.",
			LikelyFlaky: false,
		}
	case stepName == "playwright" && strings.Contains(text, "executable doesn't exist"):
		return &result.Failure{
			Category:    "harness",
			Code:        "playwright_browser_missing",
			Summary:     "The host has no browser build for this Playwright version; run npx playwright install chromium.",
			LikelyFlaky: false,
		}
	case stepName == "playwright" && strings.Contains(text, "without releasing locks"):
		return &result.Failure{
			Category:    "harness",
			Code:        "pagebuilder_render_lock",
			Summary:     "Page Builder never released its render lock, so the admin save hung. This is magento/magento2#39076, which hits at random whatever the stack.",
			LikelyFlaky: true,
		}
	case stepName == "playwright" && strings.Contains(text, "checkout.spec.ts") &&
		strings.Contains(text, "page.waitforurl") && strings.Contains(text, "/checkout/cart/\""):
		return &result.Failure{
			Category:    "harness",
			Code:        "order_success_redirect",
			Summary:     "The guest order went through but Magento sent the browser to an empty cart instead of the success page. Seen on 2.4.4 when PHP-FPM ran out of workers; it does not happen every run.",
			LikelyFlaky: true,
		}
	case stepName == "playwright" && strings.Contains(text, "page.goto: timeout"):
		return &result.Failure{
			Category:    "infrastructure",
			Code:        "storefront_timeout",
			Summary:     "The storefront did not answer the browser within the page timeout, usually because the host was overloaded.",
			LikelyFlaky: true,
		}
	case stepName == "playwright" && strings.Contains(text, "- storefront search finds the seeded product"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "search_results_missing",
			Summary:     "The storefront search did not return the seeded product, or returned results for a nonsense term, so the search engine is not serving the catalogue it was indexed with.",
			LikelyFlaky: false,
		}
	case stepName == "stack_up" && (strings.Contains(text, "already in use by container") ||
		strings.Contains(text, "endpoint with name") && strings.Contains(text, "already exists in network") ||
		strings.Contains(text, "network with name") && strings.Contains(text, "already exists")):
		return &result.Failure{
			Category:    "harness",
			Code:        "docker_name_conflict",
			Summary:     "Docker Compose found stale project resources with colliding container or network names.",
			LikelyFlaky: true,
		}
	case stepName == "stack_up" && (strings.Contains(text, "no such container") ||
		strings.Contains(text, "network has active endpoints") ||
		strings.Contains(text, "resource is still in use") ||
		(strings.Contains(text, "container ") && strings.Contains(text, " is not running"))):
		return &result.Failure{
			Category:    "harness",
			Code:        "docker_cleanup_race",
			Summary:     "Docker Compose hit a cleanup/startup race with stale containers, networks, or volumes.",
			LikelyFlaky: true,
		}
	case stepName == "stack_up" && (strings.Contains(text, "php-fpm is missing dependency") ||
		(strings.Contains(text, "failed to start") && strings.Contains(text, "exited (")) ||
		strings.Contains(text, "container ") && strings.Contains(text, " exited (") ||
		strings.Contains(text, "sigtrap")):
		return &result.Failure{
			Category:    "harness",
			Code:        "service_startup",
			Summary:     "A dependency container exited during stack startup before Magento install began.",
			LikelyFlaky: true,
		}
	case stepName == "stack_up" && strings.Contains(text, "dependency ") && strings.Contains(text, " failed to start"):
		return &result.Failure{
			Category:    "harness",
			Code:        "service_unhealthy",
			Summary:     "A dependency container started but never passed its healthcheck before stack startup gave up.",
			LikelyFlaky: true,
		}
	case stepName == "stack_up" && text == "":
		return &result.Failure{
			Category:    "harness",
			Code:        "stack_up_no_output",
			Summary:     "Docker Compose stack startup failed before the harness captured a specific error message.",
			LikelyFlaky: true,
		}
	case stepName == "install" && (strings.Contains(text, "pluginblockedexception") ||
		strings.Contains(text, "allow-plugins") ||
		strings.Contains(text, "contains a composer plugin which is blocked")):
		return &result.Failure{
			Category:    "harness",
			Code:        "composer_allow_plugins",
			Summary:     "Composer plugin execution was blocked by harness configuration.",
			LikelyFlaky: true,
		}
	case stepName == "install" && (strings.Contains(text, "block-insecure") ||
		strings.Contains(text, "security vulnerability advis") ||
		strings.Contains(text, "run composer audit")):
		return &result.Failure{
			Category:    "harness",
			Code:        "composer_audit_policy",
			Summary:     "Composer audit policy blocked the install in the harness.",
			LikelyFlaky: true,
		}
	case stepName == "smoke" && strings.Contains(text, "implicitly marking parameter") &&
		strings.Contains(text, "nullable type must be used instead"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "php84_nullable_deprecation",
			Summary:     "The product code hits PHP 8.4 implicit-nullable deprecations during compilation.",
			LikelyFlaky: false,
		}
	case (stepName == "install" || stepName == "smoke") && strings.Contains(text, "syntax error, unexpected"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "php_syntax_incompatible",
			Summary:     "The product code uses PHP syntax unsupported by the selected PHP runtime.",
			LikelyFlaky: false,
		}
	case (stepName == "install" || stepName == "smoke") &&
		strings.Contains(text, "class \"") && strings.Contains(text, "\" not found"):
		return &result.Failure{
			Category:    "compatibility",
			Code:        "compile_class_missing",
			Summary:     "The installed code references classes that are missing for this release combination.",
			LikelyFlaky: false,
		}
	default:
		return nil
	}
}

func isKnownElasticsearch8CompatibilityFailure(c matrix.Combination, log string) bool {
	return c.SearchType == "elasticsearch" &&
		strings.HasPrefix(c.SearchVersion, "8.") &&
		strings.Contains(log, "could not validate a connection to") &&
		strings.Contains(log, "no alive nodes found in") &&
		strings.Contains(log, "cluster")
}

// RunConfig holds configuration for a single combination run.
type RunConfig struct {
	ResultsDir    string
	ComposeDir    string
	PlaywrightDir string // path to tests/playwright; empty = skip playwright
	// PlaywrightImage runs the tests in this image on the stack's network,
	// with Magento installed under the service's own name. Empty runs npx on
	// the host against a published port.
	PlaywrightImage   string
	SkipCacheSave     bool
	InstallSampleData bool
	Force             bool
	MaxLogBytes       int64 // bytes to tail per container log; 0 = use default (1 MiB)

	// Deadline caps one run. When it passes, the running step fails with
	// code deadline_exceeded and the result is still written. 0 = no limit.
	Deadline time.Duration

	// OnProject, when set, gets the compose project name before the stack
	// starts, so a caller can tell its own stacks from leftovers.
	OnProject func(name string)
}

// searchConfigFlag returns the search type identifier for the Magento install
// command. Elasticsearch requires its major version as a suffix (e.g.
// "elasticsearch8"), and some Magento OpenSearch releases still expect the
// legacy "elasticsearch7" identifier.
func searchConfigFlag(c matrix.Combination) string {
	if usesLegacyMagentoOpenSearchInstall(c) {
		return "elasticsearch7"
	}
	if c.SearchType == "elasticsearch" && len(c.SearchVersion) > 0 {
		return c.SearchType + string(c.SearchVersion[0])
	}
	return c.SearchType
}

// searchHostFlagStyle selects which setup:install host/port option family to
// use. Some Magento 2.4.4/2.4.5 OpenSearch releases still require the legacy
// --elasticsearch-host/port flags even when search-engine=opensearch.
func searchHostFlagStyle(c matrix.Combination) string {
	if c.SearchType != "opensearch" {
		return "elasticsearch"
	}
	if usesLegacyMagentoOpenSearchInstall(c) {
		return "elasticsearch"
	}
	return "opensearch"
}

func usesLegacyMagentoOpenSearchInstall(c matrix.Combination) bool {
	return c.Package == "magento/project-community-edition" &&
		c.SearchType == "opensearch" &&
		compareMagentoVersion(c.Version, "2.4.6") < 0
}

func magentoVersionBetween(version, minVersion, maxVersion string) bool {
	return compareMagentoVersion(version, minVersion) >= 0 && compareMagentoVersion(version, maxVersion) <= 0
}

func compareMagentoVersion(a, b string) int {
	av, ok := parseMagentoVersion(a)
	if !ok {
		return strings.Compare(a, b)
	}
	bv, ok := parseMagentoVersion(b)
	if !ok {
		return strings.Compare(a, b)
	}

	switch {
	case av.major != bv.major:
		if av.major < bv.major {
			return -1
		}
	case av.minor != bv.minor:
		if av.minor < bv.minor {
			return -1
		}
	case av.patch != bv.patch:
		if av.patch < bv.patch {
			return -1
		}
	case av.patchLevel != bv.patchLevel:
		if av.patchLevel < bv.patchLevel {
			return -1
		}
	default:
		return 0
	}
	return 1
}

type magentoVersion struct {
	major      int
	minor      int
	patch      int
	patchLevel int
}

func parseMagentoVersion(version string) (magentoVersion, bool) {
	base := version
	patchLevel := 0

	if idx := strings.Index(version, "-p"); idx >= 0 {
		base = version[:idx]
		n, err := strconv.Atoi(version[idx+2:])
		if err != nil {
			return magentoVersion{}, false
		}
		patchLevel = n
	}

	var parsed magentoVersion
	if _, err := fmt.Sscanf(base, "%d.%d.%d", &parsed.major, &parsed.minor, &parsed.patch); err != nil {
		return magentoVersion{}, false
	}
	parsed.patchLevel = patchLevel
	return parsed, true
}

// buildMagentoEnv returns the KEY=VALUE environment pairs consumed by
// install.sh and the smoke / playwright test scripts.
func buildMagentoEnv(c matrix.Combination, searchFlag string, installSampleData bool) []string {
	sampleDataValue := "0"
	if installSampleData {
		sampleDataValue = "1"
	}
	return []string{
		"PRODUCT_PACKAGE=" + c.Package,
		"PRODUCT_VERSION=" + c.Version,
		"PHP_VERSION=" + c.PHP,
		"MIRROR_URL=" + c.Mirror,
		"DB_HOST=db",
		"DB_PORT=3306",
		"DB_NAME=magento",
		"DB_USER=magento",
		"DB_PASSWORD=magento",
		"SEARCH_TYPE=" + searchFlag,
		"SEARCH_HOST_FLAG_STYLE=" + searchHostFlagStyle(c),
		"SEARCH_HOST=search",
		"SEARCH_PORT=9200",
		"CACHE_HOST=cache",
		"CACHE_PORT=6379",
		"QUEUE_TYPE=" + c.QueueType,
		"QUEUE_HOST=queue",
		"QUEUE_PORT=" + queuePort(c),
		"QUEUE_USER=magento",
		"QUEUE_PASSWORD=magento",
		"MAGENTO_BASE_URL=http://localhost",
		"INSTALL_SAMPLE_DATA=" + sampleDataValue,
	}
}

// queuePort is the broker port Magento connects to: STOMP for Artemis, AMQP otherwise.
func queuePort(c matrix.Combination) string {
	if c.QueueType == "artemis" {
		return "61613"
	}
	return "5672"
}

// resolveBaseURL discovers the host-side mapped port and returns the base URL
// to use for the Magento install and browser tests. Varnish fronts HTTP when
// enabled, otherwise the webserver port is used. Falls back to port 80.
func resolveBaseURL(ctx context.Context, c matrix.Combination, cp *Compose) string {
	port, err := cp.Port(ctx, frontService(c), 80)
	if err != nil || port == "" {
		port = "80"
	}
	return "http://localhost:" + port
}

// frontService is the service that takes HTTP: Varnish when enabled.
func frontService(c matrix.Combination) string {
	if c.Varnish != "none" && c.Varnish != "" {
		return "varnish"
	}
	return "webserver"
}

// buildInstallArgs constructs the exec argv for running install.sh inside the
// php-fpm container with the Magento environment set and the correct base URL.
func buildInstallArgs(env []string, baseURL string) []string {
	args := make([]string, 0, len(env)+3)
	args = append(args, "env")
	args = append(args, env...)
	args = append(args, "MAGENTO_BASE_URL="+baseURL+"/")
	args = append(args, "bash", "/scripts/install.sh")
	return args
}

// composerUpstream5xx matches a package repository answering with a server
// error, e.g. `could not be downloaded (HTTP/2 504 )`.
var composerUpstream5xx = regexp.MustCompile(`could not be downloaded \(http/[0-9.]+ 5[0-9]{2}`)

func isTransientComposerNetworkFailure(log string) bool {
	log = compactWhitespace(strings.ToLower(log))
	for _, sig := range transientComposerCurlErrors {
		if strings.Contains(log, sig) {
			return true
		}
	}
	return composerUpstream5xx.MatchString(log)
}

func isRetryableInstallFailure(log string) bool {
	text := compactWhitespace(strings.ToLower(log))
	if isTransientComposerNetworkFailure(text) {
		return true
	}
	for _, sig := range retryableInstallFailureSignatures {
		if strings.Contains(text, sig) {
			return true
		}
	}
	return false
}

func compactWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func formatStackUpFailureLog(projectName, upLog, psLog, composeLogs string) string {
	var parts []string

	if trimmed := strings.TrimSpace(upLog); trimmed != "" {
		parts = append(parts, trimmed)
	}
	if trimmed := strings.TrimSpace(psLog); trimmed != "" {
		parts = append(parts, "=== docker compose ps -a ===\n"+trimmed)
	}
	if trimmed := strings.TrimSpace(composeLogs); trimmed != "" {
		parts = append(parts, "=== docker compose logs ===\n"+trimmed)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("[ERROR] docker compose up failed for project %s but produced no stdout/stderr output", projectName)
	}
	return strings.Join(parts, "\n\n")
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runInstallWithRetries(
	ctx context.Context,
	execInstall func() (string, error),
	sleep func(context.Context, time.Duration) error,
) (string, error) {
	var combined strings.Builder

	for attempt := 1; attempt <= installRetryAttempts; attempt++ {
		if attempt > 1 {
			fmt.Fprintf(&combined, "\n=== Install retry attempt %d/%d ===\n", attempt, installRetryAttempts)
		}

		out, err := execInstall()
		combined.WriteString(out)
		if err == nil {
			return combined.String(), nil
		}
		if !isRetryableInstallFailure(out) || attempt == installRetryAttempts {
			return combined.String(), err
		}

		delay := installRetryDelay * time.Duration(attempt)
		fmt.Fprintf(
			&combined,
			"\n[WARN] Retryable install failure detected; retrying install in %s (attempt %d/%d)\n",
			delay,
			attempt+1,
			installRetryAttempts,
		)
		if err := sleep(ctx, delay); err != nil {
			return combined.String(), err
		}
	}

	return combined.String(), nil
}

// Run executes the full test pipeline for one combination and writes a result
// JSON file. ran is true once the result is written. It is false, with no
// error, when the result already exists and Force is off, or when ctx is
// cancelled before the write, which leaves any earlier result untouched. A
// teardown that fails returns ErrCleanup, cancelled or not.
func Run(ctx context.Context, c matrix.Combination, cfg RunConfig) (ran bool, err error) {
	maxLog := cfg.MaxLogBytes
	if maxLog == 0 {
		maxLog = defaultMaxLogBytes
	}

	resultPath := filepath.Join(cfg.ResultsDir, c.Product, c.ID()+".json")

	if !cfg.Force {
		if _, statErr := os.Stat(resultPath); statErr == nil {
			return false, nil
		}
	}

	if ctx.Err() != nil {
		return false, nil
	}

	// ctx is the caller's: cancelling it abandons the run and writes nothing.
	// runCtx adds the deadline: passing it fails the step that was running.
	runCtx, cancelRun := ctx, context.CancelFunc(func() {})
	if cfg.Deadline > 0 {
		runCtx, cancelRun = context.WithTimeout(ctx, cfg.Deadline)
	}
	defer cancelRun()

	magentoEnv := buildMagentoEnv(c, searchConfigFlag(c), cfg.InstallSampleData)
	if cfg.SkipCacheSave {
		magentoEnv = append(magentoEnv, "CACHE_SAVE=0")
	}

	cp, err := newCompose(c, cfg.ComposeDir, magentoEnv)
	if err != nil {
		return false, fmt.Errorf("runner: building compose for %s: %w", c.ID(), err)
	}

	steps := make(map[string]result.Step)
	overallStatus := result.StatusPass

	// late: the deadline had passed when the step ended, not just by now.
	record := func(name, status string, dur float64, log string, late bool) {
		step := result.Step{Status: status, DurationS: dur, Log: log}
		if status == result.StatusFail || status == result.StatusError {
			if ctx.Err() == nil && late {
				step.Failure = &result.Failure{
					Category:    "infrastructure",
					Code:        "deadline_exceeded",
					Summary:     fmt.Sprintf("The run passed its %s deadline during this step.", cfg.Deadline),
					LikelyFlaky: true,
				}
			} else {
				step.Failure = classifyStepFailureForCombination(c, name, log)
			}
		}
		steps[name] = step
		if status != result.StatusPass && status != result.StatusSkip {
			overallStatus = result.StatusFail
		}
	}
	recordStep := func(name, status string, dur float64, log string) {
		record(name, status, dur, log, runCtx.Err() != nil)
	}

	if cfg.OnProject != nil {
		cfg.OnProject(cp.ProjectName())
	}

	wrote := false
	finish := func() (err error) {
		wrote, err = writeResult(ctx, resultPath, c, steps, overallStatus, cp, maxLog)
		return err
	}

	defer func() {
		// Fresh context: teardown must run after a cancel or a deadline too.
		downCtx, cancel := context.WithTimeout(context.Background(), CleanupTimeout)
		defer cancel()
		var rmErr error
		if cfg.PlaywrightImage != "" {
			// Killing docker run leaves its container up, holding the network.
			// rm -f of a container that's gone succeeds.
			rmErr = cp.command(downCtx, "docker", "rm", "--force", playwrightContainer(cp)).Run()
		}
		var cleanupErr error
		if downErr := errors.Join(rmErr, cp.Down(downCtx)); downErr != nil {
			cleanupErr = fmt.Errorf("runner: tearing down %s: %w: %w", cp.ProjectName(), ErrCleanup, downErr)
		} else if downCtx.Err() != nil {
			cleanupErr = fmt.Errorf("runner: tearing down %s: %w", cp.ProjectName(), ErrCleanup)
		}
		if ctx.Err() != nil && !wrote {
			ran, err = false, cleanupErr
			return
		}
		ran = ran && wrote
		err = errors.Join(err, cleanupErr)
	}()

	t := time.Now()
	upLog, upErr := cp.Up(runCtx)
	dur := time.Since(t).Seconds()
	if upErr != nil {
		late := runCtx.Err() != nil
		diagCtx, cancel := context.WithTimeout(ctx, diagnosticTimeout)
		psLog, _ := cp.run(diagCtx, "ps", "-a")
		composeLogs, _ := cp.run(diagCtx, "logs", "--no-color", "--timestamps")
		cancel()
		upLog = formatStackUpFailureLog(cp.ProjectName(), upLog, psLog, composeLogs)
		record("stack_up", result.StatusFail, dur, upLog, late)
		return true, finish()
	}
	recordStep("stack_up", result.StatusPass, dur, upLog)

	baseURL := resolveBaseURL(runCtx, c, cp)
	if cfg.PlaywrightImage != "" {
		baseURL = "http://" + frontService(c)
	}
	installArgs := buildInstallArgs(magentoEnv, baseURL)

	t = time.Now()
	installLog, installErr := runInstallWithRetries(
		runCtx,
		func() (string, error) {
			return cp.Exec(runCtx, "php-fpm", installArgs...)
		},
		sleepWithContext,
	)
	dur = time.Since(t).Seconds()
	if installErr != nil {
		recordStep("install", result.StatusFail, dur, installLog)
		return true, finish()
	}
	recordStep("install", result.StatusPass, dur, installLog)

	t = time.Now()
	smokeLog, smokeErr := cp.Exec(runCtx, "php-fpm", "bash", "/scripts/tests/smoke.sh")
	dur = time.Since(t).Seconds()
	if smokeErr != nil {
		recordStep("smoke", result.StatusFail, dur, smokeLog)
		return true, finish()
	}
	recordStep("smoke", result.StatusPass, dur, smokeLog)

	if cfg.PlaywrightDir == "" {
		recordStep("playwright", result.StatusSkip, 0, "Playwright skipped — no playwright dir configured")
	} else {
		t = time.Now()
		var pwLog string
		var pwErr error
		if cfg.PlaywrightImage != "" {
			pwLog, pwErr = runPlaywrightContainer(runCtx, cp, cfg.PlaywrightImage, cfg.PlaywrightDir, baseURL)
		} else {
			pwLog, pwErr = runPlaywright(runCtx, cfg.PlaywrightDir, baseURL, c.ID())
		}
		dur = time.Since(t).Seconds()
		if pwErr != nil {
			recordStep("playwright", result.StatusFail, dur, pwLog)
		} else {
			recordStep("playwright", result.StatusPass, dur, pwLog)
		}
	}

	for _, check := range serviceChecks(c) {
		t = time.Now()
		status, log := runServiceCheck(runCtx, cp, check)
		recordStep(check.name, status, time.Since(t).Seconds(), log)
	}

	return true, finish()
}

// serviceCheck is a script run in the php-fpm container that proves one service
// works, not just that Magento connected to it. Each one is its own step, so
// results recorded before a check existed stay valid.
type serviceCheck struct {
	name string
	argv []string
	// service is the container whose log tail is added to a failed check, for
	// causes the script cannot see, such as a broker refusing messages.
	service string
	// run replaces argv for a check that needs more than one command.
	run func(ctx context.Context, cp *Compose) (status, log string)
}

// skipExitCode is what a check script exits with when the release has nothing
// to test, for example no bulk operations.
const skipExitCode = 3

// serviceChecks lists the checks for a combination. They run after Playwright,
// so the existing steps keep the meaning they had.
func serviceChecks(c matrix.Combination) []serviceCheck {
	checks := []serviceCheck{
		{name: "queue", argv: []string{"php", "/scripts/tests/check-queue.php"}, service: "queue"},
		{name: "cron", argv: []string{"php", "/scripts/tests/check-cron.php"}},
	}
	if c.Varnish != "none" && c.Varnish != "" {
		checks = append(checks, serviceCheck{name: "varnish", run: runVarnishCheck})
	}
	return checks
}

func checkStatus(err error) string {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return result.StatusPass
	case errors.As(err, &exitErr) && exitErr.ExitCode() == skipExitCode:
		return result.StatusSkip
	default:
		return result.StatusFail
	}
}

var (
	vclRejection = regexp.MustCompile(`(?i)VCL compilation failed|Message from VCC`)
	dnsFailure   = regexp.MustCompile(`(?i)resolution error|Name or service not known|Temporary failure in name resolution|could not be resolved`)
)

var (
	varnishBackendTimeout  = 30 * time.Second
	varnishBackendInterval = time.Second
	backendHealthy         = regexp.MustCompile(`(?i)\bhealthy\b`)
	backendSick            = regexp.MustCompile(`(?i)\bsick\b`)
)

// waitForHealthyBackend polls until Varnish's probe has marked the backend healthy,
// as a new backend starts out sick. It returns the last varnishadm error, so a
// varnishadm that never answered is not taken for a sick backend.
func waitForHealthyBackend(ctx context.Context, cp *Compose) (string, bool, error) {
	deadline := time.Now().Add(varnishBackendTimeout)
	for {
		out, err := cp.run(ctx, "exec", "-T", "varnish", "varnishadm", "backend.list")
		if err == nil && backendHealthy.MatchString(out) && !backendSick.MatchString(out) {
			return out, true, nil
		}
		if time.Now().After(deadline) {
			return out, false, err
		}
		select {
		case <-ctx.Done():
			return out, false, err
		case <-time.After(varnishBackendInterval):
		}
	}
}

// runVarnishCheck points Magento at Varnish, loads the VCL into the varnish
// container, then checks caching and purging. The VCL reaches varnish through
// the host, as the two containers share no volume.
func runVarnishCheck(ctx context.Context, cp *Compose) (string, string) {
	var log strings.Builder
	record := func(title, out string) { fmt.Fprintf(&log, "=== %s ===\n%s\n", title, strings.TrimSpace(out)) }
	runCheck := func(argv ...string) (string, error) {
		return cp.Exec(ctx, "php-fpm", append([]string{"bash", "/scripts/tests/run-check.sh"}, argv...)...)
	}

	out, err := runCheck("bash", "/scripts/tests/check-varnish.sh")
	record("configure Magento for Varnish", out)
	if err != nil {
		return checkStatus(err), log.String()
	}

	dir, err := os.MkdirTemp("", "m2test-vcl-")
	if err != nil {
		record("stage the VCL", err.Error())
		return result.StatusFail, log.String()
	}
	defer os.RemoveAll(dir)
	vcl := filepath.Join(dir, "magento.vcl")

	for _, step := range []struct {
		title    string
		argv     []string
		rejected bool
	}{
		{"copy the VCL out of php-fpm", []string{"cp", "php-fpm:/scripts/tests/varnish.vcl", vcl}, false},
		{"copy it into varnish", []string{"cp", vcl, "varnish:/etc/varnish/magento.vcl"}, false},
		{"load it", []string{"exec", "-T", "varnish", "varnishadm", "vcl.load", "magento", "/etc/varnish/magento.vcl"}, true},
		{"switch Varnish to it", []string{"exec", "-T", "varnish", "varnishadm", "vcl.use", "magento"}, false},
	} {
		out, err := cp.run(ctx, step.argv...)
		record(step.title, out)
		if err == nil {
			continue
		}
		if step.rejected && vclRejection.MatchString(out) && !dnsFailure.MatchString(out) {
			fmt.Fprintln(&log, "[FAIL] varnish: Varnish rejected the VCL fixture")
		} else {
			fmt.Fprintf(&log, "[ERROR] varnish: could not %s\n", step.title)
		}
		return result.StatusFail, log.String()
	}

	out, healthy, listErr := waitForHealthyBackend(ctx, cp)
	record("wait for the backend probe", out)
	if listErr != nil {
		fmt.Fprintf(&log, "[ERROR] varnish: varnishadm backend.list kept failing: %v\n", listErr)
		return result.StatusFail, log.String()
	}
	if !healthy {
		fmt.Fprintf(&log, "[FAIL] varnish: the backend was not healthy %s after the VCL was loaded, so Varnish would mark the backend sick\n", varnishBackendTimeout)
		return result.StatusFail, log.String()
	}

	// Pages cached while Playwright ran would turn the check's first GET into a hit.
	out, err = cp.run(ctx, "exec", "-T", "varnish", "varnishadm", "ban", "req.url", "~", ".")
	record("empty the cache", out)
	if err != nil {
		fmt.Fprintf(&log, "[ERROR] varnish: could not empty the cache\n")
		return result.StatusFail, log.String()
	}

	out, err = runCheck("php", "/scripts/tests/check-varnish.php")
	record("check caching and purging", out)
	return checkStatus(err), log.String()
}

// serviceCheckTimeout backstops run-check.sh, which stops the script itself after
// 870 seconds. Killing the exec client would not stop what runs in the container.
var serviceCheckTimeout = 15 * time.Minute

func runServiceCheck(parent context.Context, cp *Compose, check serviceCheck) (string, string) {
	ctx, cancel := context.WithTimeout(parent, serviceCheckTimeout)
	defer cancel()

	status, log, killed := execServiceCheck(parent, ctx, cp, check)
	if killed && parent.Err() == nil {
		return result.StatusFail, log + fmt.Sprintf("\n[ERROR] %s: timed out after %s\n", check.name, serviceCheckTimeout)
	}
	return status, log
}

// execServiceCheck also reports whether the deadline killed the check itself.
func execServiceCheck(parent, ctx context.Context, cp *Compose, check serviceCheck) (status, log string, killed bool) {
	if check.run != nil {
		status, log = check.run(ctx, cp)
		return status, log, ctx.Err() == context.DeadlineExceeded
	}
	args := append([]string{"bash", "/scripts/tests/run-check.sh"}, check.argv...)
	log, err := cp.Exec(ctx, "php-fpm", args...)
	killed = ctx.Err() == context.DeadlineExceeded
	status = checkStatus(err)
	if status == result.StatusFail && check.service != "" {
		// The check has already failed, so the broker log gets its own short deadline.
		logCtx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		serviceLog, _ := cp.Logs(logCtx, check.service, 8<<10)
		log += fmt.Sprintf("\n=== %s container log ===\n%s", check.service, serviceLog)
	}
	return status, log, killed
}

// writeResult captures container logs then persists the result file.
func writeResult(
	ctx context.Context,
	path string,
	c matrix.Combination,
	steps map[string]result.Step,
	overallStatus string,
	cp *Compose,
	maxLog int64,
) (wrote bool, err error) {
	logCtx, cancel := context.WithTimeout(ctx, diagnosticTimeout)
	containerLogs := captureContainerLogs(logCtx, cp, maxLog)
	images := captureImages(logCtx, cp)
	cancel()

	r := result.TestResult{
		ID:            c.ID(),
		Product:       c.Product,
		Version:       c.Version,
		OverallStatus: overallStatus,
		Services:      Services(c),
		Steps:         steps,
		ContainerLogs: containerLogs,
		Images:        images,
		Workarounds:   result.ParseWorkarounds(steps["install"].Log),
		Timestamp:     result.Now(),
	}

	if ctx.Err() != nil {
		return false, nil
	}
	if err := result.Write(path, r); err != nil {
		return false, err
	}
	return true, nil
}

func Services(c matrix.Combination) result.Services {
	return result.Services{
		PHP:              c.PHP,
		Webserver:        c.WebserverType,
		WebserverVersion: c.WebserverVersion,
		DB:               result.DBService{Type: c.DBType, Version: c.DBVersion},
		Search:           result.DBService{Type: c.SearchType, Version: c.SearchVersion},
		Cache:            result.DBService{Type: c.CacheType, Version: c.CacheVersion},
		Queue:            result.DBService{Type: c.QueueType, Version: c.QueueVersion},
		Varnish:          c.Varnish,
	}
}

// captureContainerLogs collects per-service logs while the stack is still up.
// Returns nil map if the stack is already gone.
func captureContainerLogs(ctx context.Context, cp *Compose, maxLog int64) map[string]string {
	svcs, err := cp.Services(ctx)
	if err != nil || len(svcs) == 0 {
		return nil
	}

	logs := make(map[string]string, len(svcs))
	for _, svc := range svcs {
		log, _ := cp.Logs(ctx, svc, maxLog)
		logs[svc] = log
	}
	return logs
}

// playwrightEnv returns the subprocess environment for Playwright: the current
// process env with MAGENTO_BASE_URL replaced so tests hit the right stack.
// PLAYWRIGHT_BROWSERS_PATH is intentionally left alone — let it use the cache.
func playwrightEnv(baseURL, reportFile string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MAGENTO_BASE_URL=") &&
			!strings.HasPrefix(kv, "PLAYWRIGHT_REPORT_FILE=") &&
			!strings.HasPrefix(kv, "FORCE_COLOR=") &&
			!strings.HasPrefix(kv, "NO_COLOR=") {
			env = append(env, kv)
		}
	}
	return append(env,
		"MAGENTO_BASE_URL="+baseURL,
		"PLAYWRIGHT_REPORT_FILE="+reportFile,
	)
}

// runPlaywright executes the Playwright test suite on the host machine.
// playwrightDir is the path to the tests/playwright directory.
func runPlaywright(ctx context.Context, playwrightDir, baseURL, combinationID string) (string, error) {
	npx, err := exec.LookPath("npx")
	if err != nil {
		return "npx not found in PATH — cannot run Playwright", fmt.Errorf("npx not found: %w", err)
	}

	outputDir := filepath.Join("test-results", combinationID)
	reportFile := filepath.Join("playwright-report", combinationID+".json")
	cmd := Command(ctx, npx, "playwright", "test", "--project", "chromium", "--output", outputDir)
	cmd.Dir = playwrightDir
	cmd.Env = playwrightEnv(baseURL, reportFile)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err = cmd.Run()
	return summarisePlaywrightRun(buf.String(), filepath.Join(playwrightDir, reportFile), baseURL), err
}

// WaitDelay bounds how long a killed command's children, such as a compose or
// buildx plugin or the browser npx starts, can hold its output pipes open.
var WaitDelay = 10 * time.Second

func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = WaitDelay
	return cmd
}

const PlaywrightRole = "m2test.role=playwright"

func playwrightContainer(cp *Compose) string { return cp.ProjectName() + "-pw" }

// runPlaywrightContainer runs the suite in image, attached to the stack's
// network. The suite is mounted read-only; only /out is writable.
func runPlaywrightContainer(ctx context.Context, cp *Compose, image, playwrightDir, baseURL string) (string, error) {
	out, err := os.MkdirTemp("", "m2test-pw-")
	if err != nil {
		return err.Error(), err
	}
	defer os.RemoveAll(out)
	suite, err := filepath.Abs(playwrightDir)
	if err != nil {
		return err.Error(), err
	}
	// Chromium needs more than Docker's 64MB /dev/shm on heavy admin pages.
	args := []string{"run", "--rm", "--init", "--shm-size", "1g",
		"--name", playwrightContainer(cp),
		"--label", "com.docker.compose.project=" + cp.ProjectName(),
		"--label", PlaywrightRole,
		"--network", cp.ProjectName() + "_magento",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "HOME=/tmp",
		"-e", "MAGENTO_BASE_URL=" + baseURL,
		"-e", "PLAYWRIGHT_REPORT_FILE=/out/report.json",
		"-v", suite + ":/pw/suite:ro",
		"-v", out + ":/out",
		"-w", "/pw/suite",
		image,
		"/pw/node_modules/.bin/playwright", "test", "--project", "chromium", "--output", "/out/test-results",
	}
	cmd := cp.command(ctx, "docker", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	return summarisePlaywrightRun(buf.String(), filepath.Join(out, "report.json"), baseURL), err
}

type playwrightReport struct {
	Stats struct {
		Expected   int `json:"expected"`
		Skipped    int `json:"skipped"`
		Unexpected int `json:"unexpected"`
		Flaky      int `json:"flaky"`
	} `json:"stats"`
	Suites []playwrightSuite `json:"suites"`
}

type playwrightSuite struct {
	Title  string            `json:"title"`
	Specs  []playwrightSpec  `json:"specs"`
	Suites []playwrightSuite `json:"suites"`
}

type playwrightSpec struct {
	Title  string           `json:"title"`
	Status string           `json:"status"`
	Tests  []playwrightTest `json:"tests"`
}

type playwrightTest struct {
	Results []playwrightResult `json:"results"`
}

type playwrightResult struct {
	Status string `json:"status"`
}

func summarisePlaywrightRun(stdout, reportPath, baseURL string) string {
	stdout = strings.TrimSpace(stdout)

	reportSummary, err := readPlaywrightReportSummary(reportPath, baseURL)
	switch {
	case err != nil && stdout != "":
		return stdout + "\n\n[WARN] Unable to read Playwright report: " + err.Error()
	case err != nil:
		return "[WARN] Unable to read Playwright report: " + err.Error()
	case stdout == "":
		return reportSummary
	default:
		return stdout + "\n\n" + reportSummary
	}
}

func readPlaywrightReportSummary(reportPath, baseURL string) (string, error) {
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return "", err
	}

	var report playwrightReport
	if err := json.Unmarshal(data, &report); err != nil {
		return "", err
	}

	failed := collectFailedPlaywrightSpecs(report.Suites)

	var buf strings.Builder
	fmt.Fprintf(&buf, "Playwright base URL: %s\n", baseURL)
	fmt.Fprintf(&buf, "Playwright summary: expected=%d unexpected=%d flaky=%d skipped=%d\n",
		report.Stats.Expected,
		report.Stats.Unexpected,
		report.Stats.Flaky,
		report.Stats.Skipped,
	)

	if len(failed) == 0 {
		buf.WriteString("Playwright failures: none")
		return buf.String(), nil
	}

	buf.WriteString("Playwright failures:\n")
	for _, title := range failed {
		buf.WriteString(" - ")
		buf.WriteString(title)
		buf.WriteByte('\n')
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func collectFailedPlaywrightSpecs(suites []playwrightSuite) []string {
	var failed []string
	var walk func([]playwrightSuite)
	walk = func(suites []playwrightSuite) {
		for _, suite := range suites {
			for _, spec := range suite.Specs {
				if playwrightSpecFailed(spec) {
					failed = append(failed, spec.Title)
				}
			}
			if len(suite.Suites) > 0 {
				walk(suite.Suites)
			}
		}
	}
	walk(suites)
	return failed
}

func playwrightSpecFailed(spec playwrightSpec) bool {
	for _, test := range spec.Tests {
		for _, result := range test.Results {
			if result.Status == "failed" || result.Status == "timedOut" || result.Status == "interrupted" {
				return true
			}
		}
	}
	return false
}
