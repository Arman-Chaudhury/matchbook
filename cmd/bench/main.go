// Command bench measures sustained matching-engine throughput and
// per-order latency percentiles, printing machine-readable JSON.
//
// Usage: go run ./cmd/bench [-n 2000000] [-band 40] [-json]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/Arman-Chaudhury/matchbook"
)

type result struct {
	Orders       int     `json:"orders"`
	Trades       int     `json:"trades"`
	ElapsedSec   float64 `json:"elapsed_sec"`
	OrdersPerSec float64 `json:"orders_per_sec"`
	P50Us        float64 `json:"p50_us"`
	P95Us        float64 `json:"p95_us"`
	P99Us        float64 `json:"p99_us"`
	MaxUs        float64 `json:"max_us"`
	AllocMB      float64 `json:"alloc_mb"`
	GoVersion    string  `json:"go_version"`
	CPU          int     `json:"num_cpu"`
}

func main() {
	n := flag.Int("n", 2_000_000, "number of orders to process")
	band := flag.Int("band", 40, "price band width in ticks (narrower = more crossing)")
	seed := flag.Int64("seed", 42, "rng seed for reproducibility")
	asJSON := flag.Bool("json", true, "emit JSON")
	flag.Parse()

	rng := rand.New(rand.NewSource(*seed))
	orders := make([]matchbook.Order, *n)
	for i := range orders {
		o := matchbook.Order{
			ID:   int64(i + 1),
			Side: matchbook.Side(rng.Intn(2)),
			Type: matchbook.Limit,
			Qty:  int64(1 + rng.Intn(100)),
		}
		if rng.Intn(10) == 0 {
			o.Type = matchbook.Market
		} else {
			o.Price = int64(1000 + rng.Intn(*band))
		}
		orders[i] = o
	}

	eng := matchbook.NewEngine()
	lat := make([]int64, *n)
	trades := 0

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	for i, o := range orders {
		t0 := time.Now()
		trades += len(eng.Process(o))
		lat[i] = int64(time.Since(t0))
	}
	elapsed := time.Since(start)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) float64 {
		idx := int(p / 100 * float64(len(lat)-1))
		return float64(lat[idx]) / 1e3
	}

	r := result{
		Orders:       *n,
		Trades:       trades,
		ElapsedSec:   elapsed.Seconds(),
		OrdersPerSec: float64(*n) / elapsed.Seconds(),
		P50Us:        pct(50),
		P95Us:        pct(95),
		P99Us:        pct(99),
		MaxUs:        float64(lat[len(lat)-1]) / 1e3,
		AllocMB:      float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20),
		GoVersion:    runtime.Version(),
		CPU:          runtime.NumCPU(),
	}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(r)
	} else {
		fmt.Printf("%+v\n", r)
	}
}
