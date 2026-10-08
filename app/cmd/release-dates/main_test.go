package main

import (
	"reflect"
	"testing"

	"github.com/samjuk/magento-compatability/internal/matrix"
)

func TestPickKeepsOnlyTestedVersions(t *testing.T) {
	versions := []matrix.ProductVersion{{Version: "2.4.8-p5"}, {Version: "2.4.9"}, {Version: "2.4.10"}}
	known := map[string]string{"2.4.8-p5": "2026-05-12", "2.4.9": "2026-05-12", "2.4.7": "2024-04-09"}

	picked, missing := pick(versions, known)

	want := map[string]string{"2.4.8-p5": "2026-05-12", "2.4.9": "2026-05-12"}
	if !reflect.DeepEqual(picked, want) {
		t.Errorf("picked = %v, want %v", picked, want)
	}
	if !reflect.DeepEqual(missing, []string{"2.4.10"}) {
		t.Errorf("missing = %v, want [2.4.10]", missing)
	}
}

func TestDay(t *testing.T) {
	for in, want := range map[string]string{
		"2026-05-12":           "2026-05-12",
		"2024-11-05T14:02:11Z": "2024-11-05",
		"":                     "",
		"null":                 "",
		"2026-13-40":           "",
	} {
		got, ok := day(in)
		if got != want || ok != (want != "") {
			t.Errorf("day(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
