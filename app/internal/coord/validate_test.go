// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package coord

import (
	"encoding/json"
	"testing"

	"github.com/samjuk/magento-compatability/internal/result"
)

func TestValidate(t *testing.T) {
	c := combo("8.3")
	known := map[string]bool{"elasticsearch8-module": true}
	failed := func(code, cat string) result.Step {
		return result.Step{Status: result.StatusFail, Failure: &result.Failure{Code: code, Category: cat}}
	}
	cases := map[string]struct {
		edit func(*result.TestResult)
		ok   bool
	}{
		"good":               {func(*result.TestResult) {}, true},
		"known workaround":   {func(r *result.TestResult) { r.Workarounds = []string{"elasticsearch8-module"} }, true},
		"setup failure only": {func(r *result.TestResult) { *r = goodResult(c, result.StatusFail) }, true},
		"known failure code": {func(r *result.TestResult) {
			r.Steps["smoke"] = failed("deadline_exceeded", "infrastructure")
			delete(r.Steps, "playwright")
			r.OverallStatus = result.StatusFail
		}, true},
		"other combination": {func(r *result.TestResult) { r.ID = combo("8.4").ID() }, false},
		"other services":    {func(r *result.TestResult) { r.Services.DB.Version = "10.6" }, false},
		"unsafe product":    {func(r *result.TestResult) { r.Product = "../magento" }, false},
		"wrong overall":     {func(r *result.TestResult) { r.OverallStatus = result.StatusFail }, false},
		"unknown step":      {func(r *result.TestResult) { r.Steps["extra"] = result.Step{Status: result.StatusPass} }, false},
		"no stack_up":       {func(r *result.TestResult) { delete(r.Steps, "stack_up") }, false},
		"gap in steps":      {func(r *result.TestResult) { delete(r.Steps, "install") }, false},
		"cut short after a pass": {func(r *result.TestResult) {
			for _, s := range []string{"install", "smoke", "playwright"} {
				delete(r.Steps, s)
			}
		}, false},
		"cut short after a skip": {func(r *result.TestResult) {
			r.Steps["stack_up"] = result.Step{Status: result.StatusSkip}
			for _, s := range []string{"install", "smoke", "playwright"} {
				delete(r.Steps, s)
			}
		}, false},
		"smoke skipped": {func(r *result.TestResult) {
			r.Steps["smoke"] = result.Step{Status: result.StatusSkip}
			r.OverallStatus = result.OverallStatus(r.Steps)
		}, false},
		"playwright skipped": {func(r *result.TestResult) { r.Steps["playwright"] = result.Step{Status: result.StatusSkip} }, true},
		"service checks": {func(r *result.TestResult) {
			r.Steps["queue"] = result.Step{Status: result.StatusPass}
			r.Steps["cron"] = result.Step{Status: result.StatusSkip}
			r.Steps["cron"] = failed("indexer_not_updated", "compatibility")
			r.OverallStatus = result.StatusFail
		}, true},
		"varnish check without Varnish": {func(r *result.TestResult) {
			r.Steps["varnish"] = result.Step{Status: result.StatusPass}
		}, false},
		"checks after failed playwright": {func(r *result.TestResult) {
			r.Steps["playwright"] = failed("search_results_missing", "compatibility")
			r.Steps["queue"] = failed("queue_not_consumed", "compatibility")
			r.OverallStatus = result.StatusFail
		}, true},
		"check without playwright": {func(r *result.TestResult) {
			*r = goodResult(c, result.StatusFail)
			r.Steps["queue"] = result.Step{Status: result.StatusPass}
		}, false},
		"check unknown code": {func(r *result.TestResult) {
			r.Steps["queue"] = failed("made_up", "compatibility")
			r.OverallStatus = result.StatusFail
		}, false},
		"check wrong category": {func(r *result.TestResult) {
			r.Steps["cron"] = failed("cron_jobs_failed", "harness")
			r.OverallStatus = result.StatusFail
		}, false},
		"check bad status":   {func(r *result.TestResult) { r.Steps["queue"] = result.Step{Status: "maybe"} }, false},
		"bad status":         {func(r *result.TestResult) { r.Steps["smoke"] = result.Step{Status: "maybe"} }, false},
		"step after failure": {func(r *result.TestResult) { r.Steps["install"] = failed("db_version_unsupported", "compatibility") }, false},
		"unknown code":       {func(r *result.TestResult) { r.Steps["playwright"] = failed("made_up", "harness") }, false},
		"wrong category":     {func(r *result.TestResult) { r.Steps["playwright"] = failed("disk_space", "compatibility") }, false},
		"unknown workaround": {func(r *result.TestResult) { r.Workarounds = []string{"nope"} }, false},
		"odd log key":        {func(r *result.TestResult) { r.ContainerLogs = map[string]string{"../php": ""} }, false},
		"bad timestamp":      {func(r *result.TestResult) { r.Timestamp = "yesterday" }, false},
	}
	for name, tc := range cases {
		r := goodResult(c, result.StatusPass)
		tc.edit(&r)
		raw, _ := json.Marshal(r)
		if _, err := Validate(raw, c, known); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
	}

	good, _ := json.Marshal(goodResult(c, result.StatusPass))
	for name, raw := range map[string]string{
		"unknown field": string(good[:len(good)-1]) + `,"extra":1}`,
		"trailing data": string(good) + `{}`,
	} {
		if _, err := Validate([]byte(raw), c, known); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateVarnishCheck(t *testing.T) {
	c := combo("8.3")
	c.Varnish = "7.7"
	r := goodResult(c, result.StatusPass)
	r.Steps["varnish"] = result.Step{Status: result.StatusFail, Failure: &result.Failure{Code: "varnish_probe_failing", Category: "compatibility"}}
	r.OverallStatus = result.StatusFail
	raw, _ := json.Marshal(r)
	if _, err := Validate(raw, c, nil); err != nil {
		t.Fatalf("Validate() = %v, want a varnish check on a Varnish combination accepted", err)
	}
}
