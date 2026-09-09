//go:build linux

package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"runtime/trace"
	"sync"
)

func validateProfile(kind, path string) error {
	switch kind {
	case "":
		if path != "" {
			return fmt.Errorf("profile-output requires profile")
		}
	case "cpu", "allocs", "block", "mutex", "trace":
		if path == "" {
			return fmt.Errorf("profile requires an explicit profile-output path")
		}
	default:
		return fmt.Errorf("unsupported profile %q", kind)
	}
	return nil
}

// Profiles are optional and collected in separate diagnostic trials: profiling
// itself changes scheduling. CPU/trace start after the call setup barrier;
// allocs is the process-lifetime sampled allocation profile. All are flushed
// after the measurement window. No HTTP listener or automatic file is created.
func startProfile(kind, path string) func() {
	if kind == "" {
		return func() {}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	check(err)
	var stop func()
	switch kind {
	case "cpu":
		check(pprof.StartCPUProfile(f))
		stop = pprof.StopCPUProfile
	case "trace":
		check(trace.Start(f))
		stop = trace.Stop
	case "block":
		runtime.SetBlockProfileRate(1_000_000)
		stop = func() {
			runtime.SetBlockProfileRate(0)
			check(pprof.Lookup("block").WriteTo(f, 0))
		}
	case "mutex":
		old := runtime.SetMutexProfileFraction(10)
		stop = func() {
			runtime.SetMutexProfileFraction(old)
			check(pprof.Lookup("mutex").WriteTo(f, 0))
		}
	case "allocs":
		stop = func() {
			runtime.GC()
			check(pprof.Lookup("allocs").WriteTo(f, 0))
		}
	}
	return sync.OnceFunc(func() {
		stop()
		check(f.Close())
	})
}

// These quantiles are histogram bucket upper bounds, not exact observations.
// They describe sampled runnable-to-running delays across the shared sender,
// not per-call receive latency. -1 denotes an unbounded quantile bucket.
type schedulerStats struct {
	Samples uint64  `json:"samples"`
	P50MS   float64 `json:"p50_upper_ms"`
	P99MS   float64 `json:"p99_upper_ms"`
	P999MS  float64 `json:"p999_upper_ms"`
}

func schedulerDelta(before, after metrics.Float64Histogram) schedulerStats {
	var s schedulerStats
	counts := make([]uint64, len(after.Counts))
	for i, n := range after.Counts {
		counts[i] = n - before.Counts[i]
		s.Samples += counts[i]
	}
	quantile := func(q float64) float64 {
		if s.Samples == 0 {
			return 0
		}
		target := uint64(math.Ceil(float64(s.Samples) * q))
		var n uint64
		for i, count := range counts {
			n += count
			if n >= target {
				bound := after.Buckets[i+1]
				if math.IsInf(bound, 1) {
					return -1
				}
				return bound * 1000
			}
		}
		return 0
	}
	s.P50MS, s.P99MS, s.P999MS = quantile(.5), quantile(.99), quantile(.999)
	return s
}
