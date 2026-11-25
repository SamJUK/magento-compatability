// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Command m2test-worker pulls runs from a builder queue over its forwarded
// socket and sends the results back.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/samjuk/magento-compatability/internal/coord"
	"github.com/samjuk/magento-compatability/internal/images"
	"github.com/samjuk/magento-compatability/internal/queue"
	rel "github.com/samjuk/magento-compatability/internal/release"
	"github.com/samjuk/magento-compatability/internal/runner"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "prepare" {
		prepare(os.Args[2:])
		return
	}
	var (
		socket    = flag.String("socket", "", "The coordinator's worker.sock, as forwarded to this host")
		tokenFile = flag.String("token-file", "", "File holding this worker's token")
		release   = flag.String("release", "", "Release directory: the checkout this worker runs from")
		spool     = flag.String("spool", "", "Where finished runs wait to upload (default: $XDG_STATE_HOME/m2test-worker/spool)")
		slots     = flag.Int("slots", 2, "Concurrent runs")
		diskPath  = flag.String("disk-path", "/", "Filesystem to check for free space (Docker's data root on Linux)")
		minFreeGB = flag.Uint64("min-free-gb", 40, "Start no run while the disk has less than this free")
	)
	flag.Parse()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if *socket == "" || *tokenFile == "" || *release == "" {
		flag.Usage()
		os.Exit(2)
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		logger.Fatal(err)
	}
	if *spool == "" {
		*spool = filepath.Join(stateHome(), "m2test-worker", "spool")
	}
	// The release dir is usually a symlink to the current release.
	dir, err := filepath.EvalSymlinks(*release)
	if err != nil {
		logger.Fatal(err)
	}
	hash, err := rel.Hash(dir)
	if err != nil {
		logger.Fatal(err)
	}
	manifest, err := images.Load(dir)
	if err != nil {
		logger.Fatalf("%v: run m2test-worker prepare first", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	w := &coord.Worker{
		Client:  coord.NewClient(*socket, strings.TrimSpace(string(token))),
		Spool:   *spool,
		Version: hash,
		Slots:   *slots,
		Base: runner.RunConfig{
			ComposeDir:      filepath.Join(dir, "docker", "compose"),
			PlaywrightDir:   filepath.Join(dir, images.PlaywrightTests),
			PlaywrightImage: manifest.Playwright,
		},
		Runner: runner.Run,
		Log:    logger,
	}
	// Rechecked so prepare can finish, or a prune be noticed, without a
	// restart. A run started on a missing image would pull it by tag.
	// A wedged daemon would otherwise hold every claimed slot in preflight.
	missingImages := func(ctx context.Context) ([]string, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return images.Missing(ctx, hash, manifest)
	}
	lastReason := ""
	checkImages := func() {
		reason := ""
		if missing, err := missingImages(ctx); err != nil {
			reason = "checking images: " + err.Error()
		} else if len(missing) > 0 {
			reason = fmt.Sprintf("images missing: %v; run m2test-worker prepare", missing)
		}
		if reason != lastReason {
			logger.Printf("images: %s", cmp.Or(reason, "all present"))
			lastReason = reason
		}
		w.SetUnhealthy("images", reason)
		if reason == "" {
			w.SetUnhealthy("preflight", "")
		}
	}
	checkImages()
	// A prune between checks would have compose pull by tag.
	w.Preflight = func(ctx context.Context) error {
		missing, err := missingImages(ctx)
		if err == nil && len(missing) > 0 {
			err = fmt.Errorf("images missing: %v", missing)
		}
		return err
	}
	go func() {
		for t := time.NewTicker(time.Minute); ; {
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				checkImages()
			}
		}
	}()
	// Heartbeats and uploads outlive ctx so stopped runs can report.
	bg, stopBG := context.WithCancel(context.Background())
	if err := w.Start(bg); err != nil {
		logger.Fatal(err)
	}
	engine := &queue.Engine{
		Source:   w,
		MaxSlots: *slots,
		Slots:    func() int { return *slots },
		Wait:     queue.DiskGuard(*diskPath, *minFreeGB),
		Run:      w.Run,
		Log:      logger,
	}
	logger.Printf("worker up: release %.12s, %d slots, spool %s", hash, *slots, *spool)
	engine.Start(ctx)
	flush, cancel := context.WithTimeout(context.Background(), time.Minute)
	w.Flush(flush)
	cancel()
	stopBG()
	w.Wait()
	fmt.Fprintln(os.Stderr, "stopped")
}

// prepare unpacks a release, readies its images and prints its directory.
func prepare(args []string) {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	tarball := fs.String("tarball", "", "Release tarball from the coordinator")
	want := fs.String("hash", "", "Release hash the coordinator gave; the tarball must match it")
	releases := fs.String("releases", "", "Directory that holds unpacked releases")
	fs.Parse(args)
	if *tarball == "" || *releases == "" || *want == "" {
		fs.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := prepareRelease(ctx, *tarball, *releases, *want)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(dir)
}

func prepareRelease(ctx context.Context, tarball, releases, want string) (string, error) {
	before, _ := os.ReadDir(releases)
	hash, err := rel.Extract(tarball, releases)
	if err != nil {
		return "", err
	}
	// Release compose files run containers, so nothing runs before this.
	if hash != want {
		if !slices.ContainsFunc(before, func(e os.DirEntry) bool { return e.Name() == hash }) {
			os.RemoveAll(filepath.Join(releases, hash))
			os.Remove(filepath.Join(releases, hash+".tar.gz"))
		}
		return "", fmt.Errorf("%s holds release %s, not %s", tarball, hash, want)
	}
	dir := filepath.Join(releases, hash)
	m, err := images.Load(dir)
	if err != nil {
		return "", err
	}
	return dir, images.Ready(ctx, dir, hash, m)
}

func stateHome() string {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(home, ".local", "state")
}
