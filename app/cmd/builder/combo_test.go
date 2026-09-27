package main

import "testing"

func TestCombinationFailureMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *baselineEntry
		want string
	}{
		{
			name: "nil result",
			in:   nil,
			want: "result missing after run",
		},
		{
			name: "pass result",
			in:   &baselineEntry{overall: "pass"},
			want: "",
		},
		{
			name: "missing result",
			in:   &baselineEntry{overall: "missing"},
			want: "result missing after run",
		},
		{
			name: "error result",
			in:   &baselineEntry{overall: "error"},
			want: "result unreadable after run",
		},
		{
			name: "failed step",
			in:   &baselineEntry{overall: "fail", failStep: "stack_up"},
			want: "stack_up failed",
		},
		{
			name: "fallback overall status",
			in:   &baselineEntry{overall: "fail"},
			want: "overall status fail",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := combinationFailureMessage(tc.in); got != tc.want {
				t.Fatalf("combinationFailureMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}
