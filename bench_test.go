package matchbook

import (
	"math/rand"
	"testing"
)

// Benchmarks report time and allocations per operation. Compare two
// versions with benchstat:
//
//	go test -run=^$ -bench=. -benchmem -count=10 > old.txt
//	(change code)
//	go test -run=^$ -bench=. -benchmem -count=10 > new.txt
//	benchstat old.txt new.txt
//
// Order streams are generated once, outside the timer. IDs are offset on
// each pass so a long run never resubmits a resting ID.

const benchFlow = 1 << 16

// mixedFlow mirrors cmd/bench: 90% limit, 10% market, a 40-tick band, so
// most orders cross and the book stays a realistic size.
func mixedFlow(seed int64) []Order {
	rng := rand.New(rand.NewSource(seed))
	orders := make([]Order, benchFlow)
	for i := range orders {
		o := Order{Side: Side(rng.Intn(2)), Type: Limit, Qty: int64(1 + rng.Intn(100))}
		if rng.Intn(10) == 0 {
			o.Type = Market
		} else {
			o.Price = int64(1000 + rng.Intn(40))
		}
		orders[i] = o
	}
	return orders
}

func runFlow(b *testing.B, e *Engine, flow []Order) {
	b.ReportAllocs()
	b.ResetTimer()
	var base int64
	for i := 0; i < b.N; i++ {
		j := i % len(flow)
		if j == 0 && i > 0 {
			base += int64(len(flow))
		}
		o := flow[j]
		o.ID = base + int64(j) + 1
		e.Process(o)
	}
}

// BenchmarkProcessMixed is the headline number: a crossing flow where most
// orders trade and some rest.
func BenchmarkProcessMixed(b *testing.B) {
	runFlow(b, NewEngine(), mixedFlow(42))
}

// BenchmarkProcessMixedSTP is the same flow with owners and self-trade
// prevention on, to show what the check costs.
func BenchmarkProcessMixedSTP(b *testing.B) {
	flow := mixedFlow(42)
	for i := range flow {
		flow[i].Owner = int64(1 + i%8)
	}
	runFlow(b, NewEngine(WithSelfTradePrevention(STPCancelMaker)), flow)
}

// BenchmarkRestNoCross only adds liquidity: bids below 1000, asks above,
// spread across 200 levels per side. This isolates the cost of resting.
func BenchmarkRestNoCross(b *testing.B) {
	rng := rand.New(rand.NewSource(7))
	flow := make([]Order, benchFlow)
	for i := range flow {
		o := Order{Side: Side(rng.Intn(2)), Type: Limit, Qty: int64(1 + rng.Intn(100))}
		if o.Side == Buy {
			o.Price = int64(800 + rng.Intn(200))
		} else {
			o.Price = int64(1001 + rng.Intn(200))
		}
		flow[i] = o
	}
	// A fresh engine per pass keeps the book from growing without bound.
	b.ReportAllocs()
	b.ResetTimer()
	e := NewEngine()
	for i := 0; i < b.N; i++ {
		j := i % len(flow)
		if j == 0 && i > 0 {
			b.StopTimer()
			e = NewEngine()
			b.StartTimer()
		}
		o := flow[j]
		o.ID = int64(j) + 1
		e.Process(o)
	}
}

// BenchmarkRestThenCancel measures a rest followed by its cancel, the
// typical market-maker quote churn.
func BenchmarkRestThenCancel(b *testing.B) {
	e := NewEngine()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i) + 1
		e.Process(Order{ID: id, Side: Side(i & 1), Type: Limit, Price: 1000 + int64(i%20) - int64(i&1)*40, Qty: 10})
		e.Cancel(id)
	}
}

// BenchmarkReplaceReduce shrinks resting orders in place.
func BenchmarkReplaceReduce(b *testing.B) {
	e := NewEngine()
	const n = 1024
	for i := int64(1); i <= n; i++ {
		e.Process(Order{ID: i, Side: Buy, Type: Limit, Price: 900 + i%50, Qty: 1 << 40})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i%n) + 1
		e.Replace(id, 900+id%50, (1<<40)-int64(i/n)-1)
	}
}

// BenchmarkReplaceMove moves resting orders to a new price, which loses
// priority and re-enters them.
func BenchmarkReplaceMove(b *testing.B) {
	e := NewEngine()
	const n = 1024
	for i := int64(1); i <= n; i++ {
		e.Process(Order{ID: i, Side: Buy, Type: Limit, Price: 900 + i%50, Qty: 10})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i%n) + 1
		e.Replace(id, 900+int64(i%50), 10)
	}
}

// BenchmarkDepth10 takes a 10-level snapshot of a populated side.
func BenchmarkDepth10(b *testing.B) {
	e := NewEngine()
	for i := int64(1); i <= 2000; i++ {
		e.Process(Order{ID: i, Side: Buy, Type: Limit, Price: 800 + i%200, Qty: 5})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Depth(Buy, 10)
	}
}
