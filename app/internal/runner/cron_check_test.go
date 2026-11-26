// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package runner

import "testing"

func TestClassifyCronFailures(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want string
	}{
		{"failed jobs", "[FAIL] cron: cron job indexer_update_all_views ended as error: boom", "cron_jobs_failed"},
		{"triggers silent", "[FAIL] cron: saving the product left nothing in the catalog_product_price changelog, so the triggers did not fire", "indexer_not_updated"},
		{"cron never ran the index job", "[FAIL] cron: indexer_update_all_views did not finish in 2 cron runs", "indexer_not_updated"},
		{"precondition", "[ERROR] cron: the indexes already held the change before cron ran", "check_error"},
		{"search index stale", "[FAIL] cron: the opensearch index has no document for cronmarker1 after cron ran", "indexer_not_updated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStepFailure("cron", tc.log)
			if got == nil || got.Code != tc.want {
				t.Fatalf("classifyStepFailure(cron) = %#v, want code %s", got, tc.want)
			}
		})
	}
}
