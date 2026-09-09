//go:build linux

package main

import (
	"math"
	"runtime/metrics"
	"testing"
)

func TestProfileRequiresExplicitOutputAndKnownKind(t *testing.T) {
	for _, tc := range []struct {
		kind, path string
		valid      bool
	}{
		{"", "", true}, {"cpu", "", false}, {"", "unused", false},
		{"unknown", "unused", false}, {"cpu", "explicit", true},
		{"allocs", "explicit", true}, {"block", "explicit", true},
		{"mutex", "explicit", true}, {"trace", "explicit", true},
	} {
		if err := validateProfile(tc.kind, tc.path); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestSchedulerDeltaExcludesSetupAndReportsBucketBounds(t *testing.T) {
	buckets := []float64{0, .001, .01, .1, math.Inf(1)}
	before := metrics.Float64Histogram{Buckets: buckets, Counts: []uint64{0, 0, 0, 50}}
	after := metrics.Float64Histogram{Buckets: buckets, Counts: []uint64{500, 499, 1, 50}}
	s := schedulerDelta(before, after)
	if s.Samples != 1000 || s.P50MS != 1 || s.P99MS != 10 || s.P999MS != 10 {
		t.Fatal(s)
	}
	after.Counts[3]++
	if got := schedulerDelta(before, after); got.P999MS != 100 {
		t.Fatal(got)
	}
	after.Counts[3] += 100
	if got := schedulerDelta(before, after); got.P99MS != -1 {
		t.Fatal(got)
	}
	if got := schedulerDelta(before, before); got.Samples != 0 || got.P99MS != 0 {
		t.Fatal(got)
	}
}
