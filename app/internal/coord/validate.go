// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package coord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/samjuk/magento-compatability/internal/matrix"
	"github.com/samjuk/magento-compatability/internal/result"
	"github.com/samjuk/magento-compatability/internal/runner"
)

var (
	safeName    = regexp.MustCompile(`^[a-z0-9._-]{1,200}$`)
	serviceName = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	platform    = regexp.MustCompile(`^[a-z0-9]{2,20}/[a-z0-9]{2,20}(/[a-z0-9]{1,10})?$`)
	repoDigest  = regexp.MustCompile(`^[a-zA-Z0-9./_:-]{1,300}@sha256:[0-9a-f]{64}$`)
	stepOrder   = []string{"stack_up", "install", "smoke", "playwright"}
	// checkSteps run after Playwright whatever its outcome, in no fixed order.
	checkStepNames = map[string]bool{"queue": true, "cron": true, "varnish": true}
)

func LoadWorkarounds(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Workarounds []struct {
			ID string `json:"id"`
		} `json:"workarounds"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ids := map[string]bool{}
	for _, w := range f.Workarounds {
		ids[w.ID] = true
	}
	return ids, nil
}

// Validate decodes an uploaded result and checks the runner could have written it for c.
// Workers are trusted: this guards against bugs and version skew, not forgery.
func Validate(raw []byte, c matrix.Combination, workarounds map[string]bool) (result.TestResult, error) {
	var r result.TestResult
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("decoding: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return r, errors.New("trailing data after the result")
	}

	switch {
	case !safeName.MatchString(r.Product) || !safeName.MatchString(r.ID):
		return r, fmt.Errorf("unsafe product or id %q/%q", r.Product, r.ID)
	case r.ID != c.ID() || r.Product != c.Product || r.Version != c.Version:
		return r, fmt.Errorf("result is for %s, dispatched %s", r.ID, c.ID())
	case r.Services != runner.Services(c):
		return r, fmt.Errorf("services %+v don't match the combination", r.Services)
	}

	if err := checkSteps(r.Steps, c); err != nil {
		return r, err
	}
	if want := result.OverallStatus(r.Steps); r.OverallStatus != want {
		return r, fmt.Errorf("overall_status %q, steps say %q", r.OverallStatus, want)
	}
	for _, w := range r.Workarounds {
		if !workarounds[w] {
			return r, fmt.Errorf("unknown workaround %q", w)
		}
	}
	if len(r.ContainerLogs) > 20 {
		return r, fmt.Errorf("%d container logs", len(r.ContainerLogs))
	}
	for svc := range r.ContainerLogs {
		if !serviceName.MatchString(svc) {
			return r, fmt.Errorf("container log for %q", svc)
		}
	}
	if err := checkImages(r.Images); err != nil {
		return r, err
	}
	if _, err := time.Parse(time.RFC3339, r.Timestamp); err != nil {
		return r, fmt.Errorf("timestamp: %w", err)
	}
	return r, nil
}

// checkImages validates the optional image provenance. Results from before it
// existed have none, and any field may be left out.
func checkImages(images map[string]result.Image) error {
	if len(images) > 20 {
		return fmt.Errorf("%d images", len(images))
	}
	for svc, img := range images {
		switch {
		case !serviceName.MatchString(svc):
			return fmt.Errorf("image for %q", svc)
		case img.Platform != "" && !platform.MatchString(img.Platform):
			return fmt.Errorf("image %s platform %q", svc, img.Platform)
		case img.Emulated != nil && img.Platform == "":
			return fmt.Errorf("image %s is marked emulated without a platform", svc)
		case img.Digest != "" && !repoDigest.MatchString(img.Digest):
			return fmt.Errorf("image %s digest %q", svc, img.Digest)
		}
	}
	return nil
}

// checkSteps holds steps to the runner's order: each one ran only because
// the one before it passed. The service checks sit outside that chain.
func checkSteps(all map[string]result.Step, c matrix.Combination) error {
	steps := map[string]result.Step{}
	for name, st := range all {
		if !checkStepNames[name] {
			steps[name] = st
			continue
		}
		if _, ran := all["playwright"]; !ran {
			return fmt.Errorf("service check %s without a playwright step", name)
		}
		if name == "varnish" && (c.Varnish == "none" || c.Varnish == "") {
			return errors.New("varnish check on a combination without Varnish")
		}
		if err := checkStep(name, st); err != nil {
			return err
		}
	}
	return checkCore(steps)
}

func checkStep(name string, st result.Step) error {
	switch st.Status {
	case result.StatusPass, result.StatusFail, result.StatusSkip:
	default:
		return fmt.Errorf("step %s has status %q", name, st.Status)
	}
	if st.Failure != nil {
		if cat, ok := runner.FailureCodes[st.Failure.Code]; !ok || cat != st.Failure.Category {
			return fmt.Errorf("step %s has failure %s/%s", name, st.Failure.Category, st.Failure.Code)
		}
	}
	return nil
}

func checkCore(steps map[string]result.Step) error {
	known := map[string]bool{}
	for _, name := range stepOrder {
		known[name] = true
	}
	for name := range steps {
		if !known[name] {
			return fmt.Errorf("unknown step %q", name)
		}
	}
	for i, name := range stepOrder {
		st, ok := steps[name]
		if !ok {
			if i == 0 {
				return errors.New("no stack_up step")
			}
			// A run goes on until a step fails, so a short one is cut off.
			if before := steps[stepOrder[i-1]].Status; before != result.StatusFail {
				return fmt.Errorf("no %s step after %s %s", name, stepOrder[i-1], before)
			}
			for _, later := range stepOrder[i+1:] {
				if _, ok := steps[later]; ok {
					return fmt.Errorf("step %s without %s", later, name)
				}
			}
			return nil
		}
		if err := checkStep(name, st); err != nil {
			return err
		}
		if st.Status != result.StatusPass && i+1 < len(stepOrder) {
			if _, ok := steps[stepOrder[i+1]]; ok {
				return fmt.Errorf("step %s ran after %s %s", stepOrder[i+1], name, st.Status)
			}
		}
	}
	return nil
}
