// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Command m2test-worker pulls runs from a builder queue over its forwarded
// socket and sends the results back.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/samjuk/magento-compatability/internal/coord"
	"github.com/samjuk/magento-compatability/internal/queue"
	"github.com/samjuk/magento-compatability/internal/runner"
)

func main() {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	w := &coord.Worker{
		Client: coord.NewClient(*socket, strings.TrimSpace(string(token))),
		Spool:  *spool,
		// ponytail: no release hash until releases are built (phase 4).
		Slots: *slots,
		Base: runner.RunConfig{
			ComposeDir:    filepath.Join(*release, "docker", "compose"),
			PlaywrightDir: filepath.Join(*release, "docker", "scripts", "tests", "playwright"),
		},
		Runner: runner.Run,
		Log:    logger,
	}
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
	logger.Printf("worker up: %d slots, spool %s", *slots, *spool)
	engine.Start(ctx)
	flush, cancel := context.WithTimeout(context.Background(), time.Minute)
	w.Flush(flush)
	cancel()
	stopBG()
	w.Wait()
	fmt.Fprintln(os.Stderr, "stopped")
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
