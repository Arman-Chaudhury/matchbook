package matchbook

import (
	"reflect"
	"testing"
)

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
		for _, lv := range sb.heap.lvls {
			p := lv.price
			if sb.levels[p] != lv {
				t.Fatalf("heap level %d is not the map's level for that price", p)
			}
			if pooledLevels[lv] {
				t.Fatalf("live level %d is also in the pool", p)
			}
			// Walk the list forwards checking links, membership and totals.
			count, qty := 0, int64(0)
			var prev *resting
			var lastSeq uint64
			for r := lv.head; r != nil; r = r.next {
				if pooled[r] {
					t.Fatalf("level %d queue holds recycled order %p", p, r)
				}
				if r.prev != prev {
					t.Fatalf("level %d: order %d has a broken prev link", p, r.id)
				}
				if r.lv != lv || r.price != p {
					t.Fatalf("order %d in level %d points at a different level", r.id, p)
				}
				if e.byID[r.id] != r {
					t.Fatalf("order %d is queued but not indexed", r.id)
				}
				if r.qty <= 0 {
					t.Fatalf("order %d is queued with qty %d", r.id, r.qty)
				}
				if r.seq <= lastSeq {
					t.Fatalf("level %d is out of FIFO order at order %d", p, r.id)
				}
				lastSeq = r.seq
				prev = r
				count++
				qty += r.qty
			}
			if lv.tail != prev {
				t.Fatalf("level %d tail does not match the last node", p)
			}
			if count != lv.count || qty != lv.qty {
				t.Fatalf("level %d records %d orders/%d qty, list holds %d/%d", p, lv.count, lv.qty, count, qty)
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

// TestMidQueueCancelsLeaveNothingBehind pins a leak the slice-and-tombstone
// queue had: an order at the front of a level stays live while orders
// behind it are cancelled or re-priced over and over. With tombstones the
// queue grew by one dead slot per cancel; with the linked list a cancel
// unlinks and recycles its order at once.
func TestMidQueueCancelsLeaveNothingBehind(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Buy, 100, 5)) // stays at the front the whole time
	for i := int64(2); i < 20_000; i++ {
		e.Process(limit(i, Buy, 100, 1))
		if i%2 == 0 {
			e.Cancel(i)
		} else {
			e.Replace(i, 99, 1) // moves away from level 100
		}
	}
	lv := e.bids.levels[100]
	if lv.count != 1 || lv.head != lv.tail {
		t.Fatalf("level 100 should hold exactly 1 order, has %d", lv.count)
	}
	if p, q, _ := e.BestBid(); p != 100 || q != 5 {
		t.Fatalf("best bid %d/%d, want 100/5", p, q)
	}
	// FIFO must survive sweeps: order 1 first, then the oldest at 99.
	got := e.Process(market(1_000_000, Sell, 6)).Trades
	want := []Trade{{TakerID: 1_000_000, MakerID: 1, Price: 100, Qty: 5}, {TakerID: 1_000_000, MakerID: 3, Price: 99, Qty: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trades %v, want %v", got, want)
	}
	checkPool(t, e)
}

func TestMidQueueChurnDoesNotAllocate(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Buy, 100, 5))
	next := int64(2)
	churn := func() {
		for i := 0; i < 1000; i++ {
			e.Process(limit(next, Buy, 100, 1))
			e.Cancel(next)
			next++
		}
	}
	churn()
	if allocs := testing.AllocsPerRun(20, churn); allocs != 0 {
		t.Fatalf("mid-queue churn allocates %.1f times per 1000 cycles, want 0", allocs)
	}
}
