package matchbook_test

import (
	"math/rand"
	"sync"
	"testing"

	"github.com/Arman-Chaudhury/matchbook"
	"github.com/Arman-Chaudhury/matchbook/internal/flow"
)

// Benchmarks report time and allocations per operation. Compare two
// versions with benchstat:
//
//	go test -run=^$ -bench=. -benchmem -count=10 > old.txt
//	(change code)
//	go test -run=^$ -bench=. -benchmem -count=10 > new.txt
//	benchstat old.txt new.txt
//
// Streams are generated once, outside the timer.

const streamLen = 1 << 20

// Streams are built on first use, not in init, so ordinary test runs do
// not pay for generating them.
var (
	stationary = sync.OnceValue(func() []matchbook.Command {
		cmds, _ := flow.Stationary(flow.DefaultConfig(), streamLen)
		return cmds
	})
	stationarySTP = sync.OnceValue(func() []matchbook.Command {
		cfg := flow.DefaultConfig()
		cfg.Owners, cfg.STP = 8, matchbook.STPCancelMaker
		cmds, _ := flow.Stationary(cfg, streamLen)
		return cmds
	})
)

// replay applies a pre-generated command stream b.N times in total. The
// stream assumes the book state it was generated against, so at the end of
// each pass a fresh engine is swapped in with the timer stopped.
func replay(b *testing.B, cmds []matchbook.Command, opts ...matchbook.Option) {
	e := matchbook.NewEngine(opts...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(cmds)
		if j == 0 && i > 0 {
			b.StopTimer()
			e = matchbook.NewEngine(opts...)
			b.StartTimer()
		}
		e.Apply(cmds[j])
	}
}

// BenchmarkStationary is the headline number. A book of about 1,500 resting
// orders around a drifting mid; one third of commands are cancels of live
// orders, the rest are mostly passive quotes near the touch, with some
// aggressive limits and a few market orders. See internal/flow.
func BenchmarkStationary(b *testing.B) {
	replay(b, stationary())
}

// BenchmarkStationarySTP is the same shape with eight owners and
// cancel-maker self-trade prevention, to show what the check costs.
func BenchmarkStationarySTP(b *testing.B) {
	replay(b, stationarySTP(), matchbook.WithSelfTradePrevention(matchbook.STPCancelMaker))
}

// BenchmarkGrowingBook is the original benchmark flow: uniform prices over
// 40 ticks, 10% market orders and no cancels. About one order in eight rests
// forever, so the book grows without bound for the whole run. It is a
// stress test of allocation and GC under an unbounded book, not a model of
// a real market.
func BenchmarkGrowingBook(b *testing.B) {
	orders := flow.Growing(42, streamLen)
	e := matchbook.NewEngine()
	b.ReportAllocs()
	b.ResetTimer()
	var base int64
	for i := 0; i < b.N; i++ {
		j := i % len(orders)
		if j == 0 && i > 0 {
			base += int64(len(orders))
		}
		o := orders[j]
		o.ID += base
		e.Process(o)
	}
}

// BenchmarkRestNoCross only adds liquidity, spread across 200 levels per
// side, isolating the cost of resting. The book is reset every pass.
func BenchmarkRestNoCross(b *testing.B) {
	rng := rand.New(rand.NewSource(7))
	orders := make([]matchbook.Order, streamLen)
	for i := range orders {
		o := matchbook.Order{ID: int64(i + 1), Side: matchbook.Side(rng.Intn(2)), Type: matchbook.Limit, Qty: int64(1 + rng.Intn(100))}
		if o.Side == matchbook.Buy {
			o.Price = int64(800 + rng.Intn(200))
		} else {
			o.Price = int64(1001 + rng.Intn(200))
		}
		orders[i] = o
	}
	e := matchbook.NewEngine()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(orders)
		if j == 0 && i > 0 {
			b.StopTimer()
			e = matchbook.NewEngine()
			b.StartTimer()
		}
		e.Process(orders[j])
	}
}

// BenchmarkRestThenCancel measures a rest followed by its cancel, the
// typical market-maker quote churn. One op is the pair.
func BenchmarkRestThenCancel(b *testing.B) {
	e := matchbook.NewEngine()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i) + 1
		side := matchbook.Side(i & 1)
		price := int64(1000 + i%20)
		if side == matchbook.Sell {
			price += 40
		}
		e.Process(matchbook.Order{ID: id, Side: side, Type: matchbook.Limit, Price: price, Qty: 10})
		e.Cancel(id)
	}
}

// BenchmarkReplaceReduce shrinks resting orders in place.
func BenchmarkReplaceReduce(b *testing.B) {
	e := matchbook.NewEngine()
	const n = 1024
	for i := int64(1); i <= n; i++ {
		e.Process(matchbook.Order{ID: i, Side: matchbook.Buy, Type: matchbook.Limit, Price: 900 + i%50, Qty: 1 << 40})
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
	e := matchbook.NewEngine()
	const n = 1024
	for i := int64(1); i <= n; i++ {
		e.Process(matchbook.Order{ID: i, Side: matchbook.Buy, Type: matchbook.Limit, Price: 900 + i%50, Qty: 10})
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
	e := matchbook.NewEngine()
	for i := int64(1); i <= 2000; i++ {
		e.Process(matchbook.Order{ID: i, Side: matchbook.Buy, Type: matchbook.Limit, Price: 800 + i%200, Qty: 5})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Depth(matchbook.Buy, 10)
	}
}
