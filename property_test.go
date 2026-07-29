package matchbook

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// refBook is a deliberately naive O(n) reference implementation. The
// property suite drives the real engine and this model with identical
// random operation streams and requires byte-identical behaviour.
type refBook struct {
	orders []*refOrder
	seq    uint64
}

type refOrder struct {
	id    int64
	side  Side
	price int64
	qty   int64
	seq   uint64
}

func (rb *refBook) bestMaker(taker Side) *refOrder {
	var best *refOrder
	for _, o := range rb.orders {
		if o.side != taker.Opposite() || o.qty == 0 {
			continue
		}
		if best == nil {
			best = o
			continue
		}
		better := false
		if taker == Buy { // matching against asks: lowest price wins
			better = o.price < best.price || (o.price == best.price && o.seq < best.seq)
		} else { // matching against bids: highest price wins
			better = o.price > best.price || (o.price == best.price && o.seq < best.seq)
		}
		if better {
			best = o
		}
	}
	return best
}

func (rb *refBook) process(o Order) []Trade {
	if o.Qty <= 0 || (o.Type == Limit && o.Price <= 0) {
		return nil
	}
	var trades []Trade
	for o.Qty > 0 {
		m := rb.bestMaker(o.Side)
		if m == nil || !crosses(o.Side, o.Type, o.Price, m.price) {
			break
		}
		qty := min(o.Qty, m.qty)
		trades = append(trades, Trade{TakerID: o.ID, MakerID: m.id, Price: m.price, Qty: qty})
		o.Qty -= qty
		m.qty -= qty
	}
	if o.Qty > 0 && o.Type == Limit {
		rb.seq++
		rb.orders = append(rb.orders, &refOrder{
			id: o.ID, side: o.Side, price: o.Price, qty: o.Qty, seq: rb.seq,
		})
	}
	return trades
}

func (rb *refBook) cancel(id int64) bool {
	for _, o := range rb.orders {
		if o.id == id && o.qty > 0 {
			o.qty = 0
			return true
		}
	}
	return false
}

func (rb *refBook) depth(side Side, n int) []Depth {
	agg := map[int64]int64{}
	for _, o := range rb.orders {
		if o.side == side && o.qty > 0 {
			agg[o.price] += o.qty
		}
	}
	prices := make([]int64, 0, len(agg))
	for p := range agg {
		prices = append(prices, p)
	}
	sort.Slice(prices, func(i, j int) bool {
		if side == Buy {
			return prices[i] > prices[j]
		}
		return prices[i] < prices[j]
	})
	out := []Depth{}
	for _, p := range prices {
		if len(out) == n {
			break
		}
		out = append(out, Depth{Price: p, Qty: agg[p]})
	}
	return out
}

const (
	propCases  = 300 // independent random scenarios
	propOps    = 400 // operations per scenario
	priceBand  = 20  // narrow band forces heavy crossing
	priceFloor = 90
)

// TestPropertyEngineMatchesReferenceModel is the core property suite:
// 300 random scenarios x 400 operations, engine vs. reference model.
func TestPropertyEngineMatchesReferenceModel(t *testing.T) {
	for c := 0; c < propCases; c++ {
		c := c
		t.Run(fmt.Sprintf("case%03d", c), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(c)))
			eng := NewEngine()
			ref := &refBook{}
			var nextID int64
			var live []int64

			for op := 0; op < propOps; op++ {
				switch {
				case rng.Intn(10) == 0 && len(live) > 0: // cancel a known id
					id := live[rng.Intn(len(live))]
					if got, want := eng.Cancel(id), ref.cancel(id); got != want {
						t.Fatalf("op %d: cancel(%d) = %v, ref %v", op, id, got, want)
					}
				default:
					nextID++
					o := Order{
						ID:   nextID,
						Side: Side(rng.Intn(2)),
						Type: Limit,
						Qty:  int64(1 + rng.Intn(50)),
					}
					if rng.Intn(8) == 0 {
						o.Type = Market
					} else {
						o.Price = int64(priceFloor + rng.Intn(priceBand))
						live = append(live, o.ID)
					}
					got := append([]Trade(nil), eng.Process(o)...)
					want := ref.process(o)
					if len(got) != 0 || len(want) != 0 {
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("op %d: order %+v\nengine: %v\nref:    %v", op, o, got, want)
						}
					}
				}

				// Invariant: book is never crossed.
				if bid, _, ok1 := eng.BestBid(); ok1 {
					if ask, _, ok2 := eng.BestAsk(); ok2 && bid >= ask {
						t.Fatalf("op %d: crossed book bid=%d ask=%d", op, bid, ask)
					}
				}
			}

			// Final depth must agree on both sides.
			for _, side := range []Side{Buy, Sell} {
				got, want := eng.Depth(side, 10), ref.depth(side, 10)
				if len(got) == 0 && len(want) == 0 {
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("final %v depth:\nengine: %v\nref:    %v", side, got, want)
				}
			}
		})
	}
}

// TestPropertyConservation checks that every unit of submitted quantity is
// accounted for: filled twice (maker+taker), resting, cancelled, or
// discarded market remainder.
func TestPropertyConservation(t *testing.T) {
	for c := 0; c < 50; c++ {
		rng := rand.New(rand.NewSource(int64(1000 + c)))
		eng := NewEngine()
		var submitted, filled, cancelled, discarded int64
		var nextID int64
		type rec struct {
			qty    int64
			market bool
		}
		remaining := map[int64]*rec{}

		for op := 0; op < 500; op++ {
			nextID++
			o := Order{ID: nextID, Side: Side(rng.Intn(2)), Type: Limit,
				Qty: int64(1 + rng.Intn(50)), Price: int64(90 + rng.Intn(20))}
			if rng.Intn(8) == 0 {
				o.Type = Market
				o.Price = 0
			}
			submitted += o.Qty
			remaining[o.ID] = &rec{qty: o.Qty, market: o.Type == Market}
			for _, tr := range eng.Process(o) {
				filled += 2 * tr.Qty
				remaining[tr.TakerID].qty -= tr.Qty
				remaining[tr.MakerID].qty -= tr.Qty
			}
			if r := remaining[o.ID]; r.market && r.qty > 0 {
				discarded += r.qty
				r.qty = 0
			}
			if rng.Intn(12) == 0 {
				id := int64(1 + rng.Intn(int(nextID)))
				if eng.Cancel(id) {
					cancelled += remaining[id].qty
					remaining[id].qty = 0
				}
			}
		}
		var resting int64
		for _, r := range remaining {
			resting += r.qty
		}
		if submitted != filled+resting+cancelled+discarded {
			t.Fatalf("case %d: conservation violated: submitted=%d filled=%d resting=%d cancelled=%d discarded=%d",
				c, submitted, filled, resting, cancelled, discarded)
		}
	}
}
