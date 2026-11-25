// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package queue

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samjuk/magento-compatability/internal/runner"
)

var projectName = regexp.MustCompile(`^m2test-[0-9a-f]{16}-[0-9a-f]{8}`)

// dockerTimeout bounds each docker call here, so a wedged daemon stalls a
// slot for minutes rather than for good.
const dockerTimeout = 2 * time.Minute

var command = runner.Command

// RemoveStack tears down a compose project by name, then any volumes older Compose versions leave behind.
// A Playwright container on the network goes first, or the network can't be removed.
func RemoveStack(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, runner.CleanupTimeout)
	defer cancel()
	out, err := command(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+name, "--filter", "label="+runner.PlaywrightRole).Output()
	if err != nil {
		return fmt.Errorf("listing test containers of %s: %w", name, err)
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		if out, err := command(ctx, "docker", append([]string{"rm", "--force"}, ids...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("removing test containers of %s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	out, err = command(ctx, "docker", "compose", "-p", name, "down", "--volumes", "--remove-orphans").CombinedOutput()
	if err != nil {
		return fmt.Errorf("removing %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	vols, err := stackVolumes(ctx, name)
	if err != nil {
		return err
	}
	if len(vols) > 0 {
		out, err := command(ctx, "docker", append([]string{"volume", "rm", "--force"}, vols...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("removing volumes of %s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	// Compose succeeds yet leaves a network another container holds.
	up, err := StackExists(ctx, name)
	if err == nil && up {
		err = fmt.Errorf("%s still there after teardown", name)
	}
	return err
}

func StackExists(ctx context.Context, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	out, err := command(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+name).Output()
	if err != nil {
		return false, fmt.Errorf("docker ps: %w", err)
	}
	vols, err := stackVolumes(ctx, name)
	if err != nil {
		return false, err
	}
	// A network still in use by a stranger outlives its stack's teardown.
	nets, err := command(ctx, "docker", "network", "ls", "-q", "--filter", "label=com.docker.compose.project="+name).Output()
	if err != nil {
		return false, fmt.Errorf("docker network ls: %w", err)
	}
	return len(bytes.TrimSpace(out)) > 0 || len(vols) > 0 || len(bytes.TrimSpace(nets)) > 0, nil
}

// stackVolumes lists a stack's volumes, which runner names <project>_<name>.
// Docker's name filter matches substrings, hence the prefix check.
func stackVolumes(ctx context.Context, name string) ([]string, error) {
	out, err := command(ctx, "docker", "volume", "ls", "-q", "--filter", "name="+name+"_").Output()
	if err != nil {
		return nil, fmt.Errorf("listing volumes of %s: %w", name, err)
	}
	return ownVolumes(name, strings.Fields(string(out))), nil
}

func ownVolumes(project string, listed []string) []string {
	var own []string
	for _, v := range listed {
		if strings.HasPrefix(v, project+"_") {
			own = append(own, v)
		}
	}
	return own
}

type MemorySampler struct {
	mu    sync.Mutex
	peaks map[string]uint64
}

func (s *MemorySampler) Start(ctx context.Context, every time.Duration) {
	s.peaks = map[string]uint64{}
	go func() {
		for {
			s.sample(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(every):
			}
		}
	}()
}

func (s *MemorySampler) Take(project string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peaks[project]
	delete(s.peaks, project)
	return p
}

func (s *MemorySampler) sample(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	out, err := command(ctx, "docker", "stats", "--no-stream", "--format", "{{.Name}}|{{.MemUsage}}").Output()
	if err != nil {
		return
	}
	totals := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name, usage, _ := strings.Cut(sc.Text(), "|")
		project := projectName.FindString(name)
		if project == "" {
			continue
		}
		used, _, _ := strings.Cut(usage, " / ")
		totals[project] += parseSize(used)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, n := range totals {
		if n > s.peaks[p] {
			s.peaks[p] = n
		}
	}
}

var sizeUnits = map[string]float64{
	"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	"kB": 1e3, "MB": 1e6, "GB": 1e9,
}

func parseSize(s string) uint64 {
	s = strings.TrimSpace(s)
	i := strings.LastIndexAny(s, "0123456789.") + 1
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	return uint64(n * sizeUnits[s[i:]])
}
