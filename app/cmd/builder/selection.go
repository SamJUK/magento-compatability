// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package main

import (
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/runner"
)

type selection struct {
	filter     matrix.Filter
	baseline   bool
	matrixFile string
	resultsDir string
	composeDir string
	repoRoot   string

	playwright  bool
	sampleData  bool
	noCacheSave bool
	maxLogBytes int64
	deadline    time.Duration
}

func addSelectionFlags(fs *flag.FlagSet, deadline time.Duration) *selection {
	s := &selection{}
	fs.StringVar(&s.filter.Product, "product", "", "Filter by product name (magento|mageos)")
	fs.StringVar(&s.filter.Version, "version", "", "Filter by product version (e.g. 2.4.8)")
	fs.StringVar(&s.filter.PHP, "php", "", "Filter by PHP version (e.g. 8.3)")
	fs.StringVar(&s.filter.Webserver, "webserver", "", "Filter by webserver type (nginx|apache)")
	fs.StringVar(&s.filter.DB, "db", "", "Filter by database (type:version, e.g. mariadb:11.4)")
	fs.StringVar(&s.filter.Search, "search", "", "Filter by search engine (type:version, e.g. opensearch:3)")
	fs.StringVar(&s.filter.Cache, "cache", "", "Filter by cache (type:version, e.g. valkey:8)")
	fs.StringVar(&s.filter.Queue, "queue", "", "Filter by queue (type:version, e.g. rabbitmq:4.2)")
	fs.StringVar(&s.filter.Varnish, "varnish", "", "Filter by varnish version or \"none\"")
	fs.BoolVar(&s.baseline, "baseline", false, "Run only the baseline combination(s)")
	fs.StringVar(&s.matrixFile, "matrix", "", "Path to matrix.yml (default: auto-detect from repo root)")
	fs.StringVar(&s.resultsDir, "results-dir", "", "Path to results directory (default: <repo-root>/results)")
	fs.StringVar(&s.composeDir, "compose-dir", "", "Path to compose directory (default: <repo-root>/docker/compose)")
	fs.BoolVar(&s.playwright, "playwright", true, "Run Playwright E2E tests after smoke tests")
	fs.BoolVar(&s.sampleData, "sample-data", false, "Install Magento sample data before smoke/Playwright validation")
	fs.BoolVar(&s.noCacheSave, "no-cache-save", false, "Don't grow the shared Composer and vendor caches (saves disk on one-off sweeps)")
	fs.Int64Var(&s.maxLogBytes, "max-log-bytes", 1<<20, "Maximum bytes to capture per container log (0 = unlimited)")
	fs.DurationVar(&s.deadline, "deadline", deadline, "Fail a combination that runs longer than this (0 = no limit)")
	return s
}

func (s *selection) load() []matrix.Combination {
	repoRoot := findRepoRoot()
	s.repoRoot = repoRoot
	if s.matrixFile == "" {
		s.matrixFile = filepath.Join(repoRoot, "matrix.yml")
	}
	if s.resultsDir == "" {
		s.resultsDir = filepath.Join(repoRoot, "results")
	}
	if s.composeDir == "" {
		s.composeDir = filepath.Join(repoRoot, "docker", "compose")
	}
	// Lease keys resolve symlinks, which needs the directory to exist.
	if err := os.MkdirAll(s.resultsDir, 0o755); err != nil {
		fatalf("%v", err)
	}

	m, err := matrix.Load(s.matrixFile)
	if err != nil {
		fatalf("loading matrix: %v", err)
	}
	if s.baseline {
		return matrix.BuildBaselineCombinations(m, s.filter)
	}
	return matrix.BuildCombinations(m, s.filter)
}

func (s *selection) contract() queue.Contract {
	return queue.Contract{
		Playwright:  s.playwright,
		SampleData:  s.sampleData,
		MaxLogBytes: s.maxLogBytes,
		Deadline:    s.deadline,
	}
}

func (s *selection) runConfig() runner.RunConfig {
	cfg := runner.RunConfig{
		ResultsDir:        s.resultsDir,
		ComposeDir:        s.composeDir,
		InstallSampleData: s.sampleData,
		SkipCacheSave:     s.noCacheSave,
		MaxLogBytes:       s.maxLogBytes,
		Deadline:          s.deadline,
	}
	if s.playwright {
		cfg.PlaywrightDir = filepath.Join(s.repoRoot, "docker", "scripts", "tests", "playwright")
	}
	return cfg
}
