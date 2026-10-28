package matchbook

import "testing"

// checkPool asserts the recycling invariants: nothing is in the pool twice,
// and nothing in the pool is still reachable, either through the ID index
// or from a level's live queue (head onward). A violation means a recycled
// order could be handed out while the book still points at it.
func checkPool(t *testing.T, e *Engine) {
	t.Helper()
	pooled := make(map[*resting]bool, len(e.pool.orders))
	for _, r := range e.pool.orders {
		if pooled[r] {
			t.Fatalf("order %p recycled twice", r)
		}
		pooled[r] = true
	}
	for id, r := range e.byID {
		if pooled[r] {
			t.Fatalf("resting id %d is also in the pool", id)
		}
	}
	pooledLevels := map[*level]bool{}
	for _, lv := range e.pool.levels {
		if pooledLevels[lv] {
			t.Fatalf("level %p recycled twice", lv)
		}
		pooledLevels[lv] = true
	}
	for _, sb := range []*sideBook{e.bids, e.asks} {
		if len(sb.levels) != sb.heap.Len() {
			t.Fatalf("%d levels but %d heap entries", len(sb.levels), sb.heap.Len())
		}
		for _, p := range sb.heap.prices {
			lv, ok := sb.levels[p]
			if !ok {
				t.Fatalf("heap price %d has no level", p)
			}
			if pooledLevels[lv] {
				t.Fatalf("live level %d is also in the pool", p)
			}
			for _, r := range lv.orders[lv.head:] {
				if pooled[r] {
					t.Fatalf("level %d queue holds recycled order %p", p, r)
				}
			}
		}
	}
}

func TestPoolInvariantsUnderRandomStreams(t *testing.T) {
	for seed := int64(0); seed < 30; seed++ {
		e := NewEngine(WithSelfTradePrevention(STPMode(seed % 3)))
		for i, c := range randomCommands(500+seed, 3000) {
			e.Apply(c)
			if i%100 == 0 {
				checkPool(t, e)
			}
		}
		checkPool(t, e)
		// Drain everything so every order and level goes back to the pool.
		for id := range e.byID {
			e.Cancel(id)
		}
		e.BestBid()
		e.BestAsk()
		checkPool(t, e)
		if len(e.bids.levels)+len(e.asks.levels) != 0 {
			t.Fatalf("seed %d: levels left after draining", seed)
		}
	}
}

func TestSteadyStateDoesNotAllocate(t *testing.T) {
	e := NewEngine()
	warm := func(base int64) {
		for i := int64(0); i < 2000; i++ {
			id := base + i
			e.Process(Order{ID: id, Side: Side(i & 1), Type: Limit, Price: 1000 + i%20 - (i&1)*40, Qty: 10})
			e.Cancel(id)
		}
	}
	warm(1)
	var base int64 = 1_000_000
	allocs := testing.AllocsPerRun(50, func() {
		warm(base)
		base += 10_000
	})
	if allocs != 0 {
		t.Fatalf("rest+cancel churn allocates %.1f times per 2000 cycles after warm-up, want 0", allocs)
	}
}
