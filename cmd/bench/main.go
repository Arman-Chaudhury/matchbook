// Command bench measures matching-engine throughput, per-command latency
// and garbage-collection cost on a deterministic order flow.
//
// It makes three passes over the same pre-generated stream, each into a
// fresh engine:
//
//  1. Throughput: no timing inside the loop, so the clock costs nothing.
//     Allocation and GC figures come from this pass.
//  2. Latency: every command is timed individually. Reading the clock twice
//     costs tens of nanoseconds, so the pass also reports that overhead,
//     measured by timing an empty operation the same way. Percentiles are
//     shown as measured, overhead included.
//  3. A replay check: the digest of the throughput pass must equal the
//     digest of the latency pass, proving both measured the same work.
//
// Usage:
//
//	go run ./cmd/bench                      # stationary flow, 2M commands
//	go run ./cmd/bench -flow growing        # the original unbounded-book flow
//	go run ./cmd/bench -n 5000000 -json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"time"

	"github.com/Arman-Chaudhury/matchbook"
	"github.com/Arman-Chaudhury/matchbook/internal/flow"
)

type result struct {
	Flow           string  `json:"flow"`
	Commands       int     `json:"commands"`
	Submits        int     `json:"submits"`
	Cancels        int     `json:"cancels"`
	Trades         int     `json:"trades"`
	MaxResting     int     `json:"max_resting_orders,omitempty"`
	CommandsPerSec float64 `json:"commands_per_sec"`
	NsPerCommand   float64 `json:"ns_per_command"`
	P50Ns          int64   `json:"p50_ns"`
	P99Ns          int64   `json:"p99_ns"`
	P999Ns         int64   `json:"p999_ns"`
	MaxNs          int64   `json:"max_ns"`
	TimerNs        float64 `json:"timer_overhead_mean_ns"`
	ClockResNs     int64   `json:"clock_resolution_ns"`
	BytesPerCmd    float64 `json:"alloc_bytes_per_command"`
	GCRuns         uint32  `json:"gc_runs"`
	GCPauseTotalUs float64 `json:"gc_pause_total_us"`
	GCPauseMaxUs   float64 `json:"gc_pause_max_us"`
	Digest         string  `json:"digest"`
	GoVersion      string  `json:"go_version"`
	GOOS           string  `json:"goos"`
	GOARCH         string  `json:"goarch"`
	CPUs           int     `json:"num_cpu"`
}

func main() {
	n := flag.Int("n", 2_000_000, "commands to process")
	flowName := flag.String("flow", "stationary", "stationary or growing")
	seed := flag.Int64("seed", 42, "rng seed")
	depth := flag.Int("depth", flow.DefaultConfig().TargetDepth, "stationary flow: target resting orders")
	asJSON := flag.Bool("json", false, "emit JSON instead of a table")
	flag.Parse()

	cmds, r := buildFlow(*flowName, *seed, *depth, *n)

	// Pass 1: throughput, GC and allocation.
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	eng := matchbook.NewEngine()
	trades := 0
	start := time.Now()
	for _, c := range cmds {
		for _, ev := range eng.Apply(c) {
			if ev.Kind == matchbook.EvFill {
				trades++
			}
		}
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	digest := eng.Digest()

	r.Trades = trades
	r.CommandsPerSec = float64(len(cmds)) / elapsed.Seconds()
	r.NsPerCommand = float64(elapsed.Nanoseconds()) / float64(len(cmds))
	r.BytesPerCmd = float64(after.TotalAlloc-before.TotalAlloc) / float64(len(cmds))
	r.GCRuns = after.NumGC - before.NumGC
	r.GCPauseTotalUs = float64(after.PauseTotalNs-before.PauseTotalNs) / 1e3
	r.GCPauseMaxUs = float64(maxPause(&after, before.NumGC)) / 1e3

	// Pass 2: per-command latency.
	lat := make([]int64, len(cmds))
	eng = matchbook.NewEngine()
	for i, c := range cmds {
		t0 := time.Now()
		eng.Apply(c)
		lat[i] = int64(time.Since(t0))
	}
	if eng.Digest() != digest {
		fmt.Fprintln(os.Stderr, "bench: latency pass diverged from throughput pass")
		os.Exit(1)
	}
	slices.Sort(lat)
	r.P50Ns, r.P99Ns, r.P999Ns, r.MaxNs = pct(lat, 50), pct(lat, 99), pct(lat, 99.9), lat[len(lat)-1]
	r.TimerNs, r.ClockResNs = timerCost()
	r.Digest = fmt.Sprintf("%016x", digest)
	r.GoVersion, r.GOOS, r.GOARCH, r.CPUs = runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU()

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(r)
		return
	}
	printTable(r)
}

func buildFlow(name string, seed int64, depth, n int) ([]matchbook.Command, result) {
	switch name {
	case "stationary":
		cfg := flow.DefaultConfig()
		cfg.Seed, cfg.TargetDepth = seed, depth
		cmds, st := flow.Stationary(cfg, n)
		return cmds, result{Flow: name, Commands: n, Submits: st.Submits, Cancels: st.Cancels, MaxResting: st.MaxResting}
	case "growing":
		orders := flow.Growing(seed, n)
		cmds := make([]matchbook.Command, n)
		for i, o := range orders {
			cmds[i] = matchbook.Command{Kind: matchbook.CmdSubmit, Order: o}
		}
		return cmds, result{Flow: name, Commands: n, Submits: n}
	}
	fmt.Fprintf(os.Stderr, "bench: unknown flow %q (want stationary or growing)\n", name)
	os.Exit(2)
	return nil, result{}
}

// maxPause returns the longest GC pause among cycles after sinceGC. The
// runtime keeps the last 256 pauses; older ones are not visible.
func maxPause(m *runtime.MemStats, sinceGC uint32) uint64 {
	var worst uint64
	for gc := sinceGC + 1; gc <= m.NumGC && m.NumGC-gc < 256; gc++ {
		if p := m.PauseNs[(gc+255)%256]; p > worst {
			worst = p
		}
	}
	return worst
}

func pct(sorted []int64, p float64) int64 {
	return sorted[int(p/100*float64(len(sorted)-1))]
}

// timerCost times an empty operation the same way the latency pass times
// a command. It returns the mean cost, which stays meaningful when the
// clock is coarse, and the clock's resolution: the smallest non-zero step
// observed. Every latency figure is a multiple of that resolution.
func timerCost() (meanNs float64, resolutionNs int64) {
	const reps = 500_000
	var sum int64
	resolutionNs = 1 << 62
	for i := 0; i < reps; i++ {
		t0 := time.Now()
		d := int64(time.Since(t0))
		sum += d
		if d > 0 && d < resolutionNs {
			resolutionNs = d
		}
	}
	return float64(sum) / reps, resolutionNs
}

func printTable(r result) {
	fmt.Printf("flow            %s (%d commands: %d submits, %d cancels)\n", r.Flow, r.Commands, r.Submits, r.Cancels)
	if r.MaxResting > 0 {
		fmt.Printf("book            peak %d resting orders\n", r.MaxResting)
	}
	fmt.Printf("trades          %d\n", r.Trades)
	fmt.Printf("throughput      %.2fM commands/sec (%.1f ns/command, no timer in loop)\n", r.CommandsPerSec/1e6, r.NsPerCommand)
	fmt.Printf("latency         p50 %d ns  p99 %d ns  p99.9 %d ns  max %d ns\n", r.P50Ns, r.P99Ns, r.P999Ns, r.MaxNs)
	fmt.Printf("clock           %d ns resolution, %.1f ns mean cost per measurement; latencies include it and are multiples of the resolution\n", r.ClockResNs, r.TimerNs)
	fmt.Printf("allocation      %.2f bytes/command (%.1f MB total)\n", r.BytesPerCmd, r.BytesPerCmd*float64(r.Commands)/(1<<20))
	fmt.Printf("gc              %d runs, %.0f µs total pause, %.0f µs longest\n", r.GCRuns, r.GCPauseTotalUs, r.GCPauseMaxUs)
	fmt.Println("                (the pre-generated stream sits on the heap and raises the GC trigger;")
	fmt.Println("                 allocation per command is the figure that carries over to a real process)")
	fmt.Printf("digest          %s\n", r.Digest)
	fmt.Printf("runtime         %s %s/%s, %d CPUs\n", r.GoVersion, r.GOOS, r.GOARCH, r.CPUs)
}
