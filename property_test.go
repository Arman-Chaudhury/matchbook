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
// random operation streams and requires byte-identical behaviour. It knows
// every rule the engine knows (time in force, post-only, self-trade
// prevention, replace priority) but expresses each as a linear scan over a
// flat list, so a shared bug would have to be a shared misunderstanding of
// the rule rather than a shared data-structure slip.
type refBook struct {
	orders []*refOrder
	seq    uint64
	stp    STPMode
}

type refOrder struct {
	id    int64
	owner int64
	side  Side
	price int64
	qty   int64
	seq   uint64
}

// better reports whether a beats b as a maker for a taker on side taker.
func better(taker Side, a, b *refOrder) bool {
	if taker == Buy { // matching against asks: lowest price wins
		return a.price < b.price || (a.price == b.price && a.seq < b.seq)
	}
	return a.price > b.price || (a.price == b.price && a.seq < b.seq)
}

func (rb *refBook) bestMaker(taker Side) *refOrder {
	var best *refOrder
	for _, o := range rb.orders {
		if o.side != taker.Opposite() || o.qty == 0 {
			continue
		}
		if best == nil || better(taker, o, best) {
			best = o
		}
	}
	return best
}

func (rb *refBook) find(id int64) *refOrder {
	for _, o := range rb.orders {
		if o.id == id && o.qty > 0 {
			return o
		}
	}
	return nil
}

func (rb *refBook) selfTrade(o Order, m *refOrder) bool {
	return o.Owner != 0 && m.owner == o.Owner && rb.stp != STPNone
}

// available mirrors Engine.available: liquidity o could take right now.
func (rb *refBook) available(o Order) int64 {
	var makers []*refOrder
	for _, m := range rb.orders {
		if m.side == o.Side.Opposite() && m.qty > 0 && crosses(o.Side, o.Type, o.Price, m.price) {
			makers = append(makers, m)
		}
	}
	sort.Slice(makers, func(i, j int) bool { return better(o.Side, makers[i], makers[j]) })
	var sum int64
	for _, m := range makers {
		if rb.selfTrade(o, m) {
			if rb.stp == STPCancelTaker {
				break
			}
			continue
		}
		sum += m.qty
	}
	return sum
}

func (rb *refBook) process(o Order) Result {
	if r := validate(o); r != RejectNone {
		return Result{Status: Rejected, Reject: r}
	}
	if rb.find(o.ID) != nil {
		return Result{Status: Rejected, Reject: RejectDuplicateID}
	}
	if o.PostOnly {
		if m := rb.bestMaker(o.Side); m != nil && crosses(o.Side, o.Type, o.Price, m.price) {
			return Result{Status: Rejected, Reject: RejectWouldCross}
		}
	}
	if o.Type == Limit && o.TIF == FOK && rb.available(o) < o.Qty {
		return Result{Status: Discarded, Remaining: o.Qty}
	}
	var trades []Trade
	selfHit := false
	for o.Qty > 0 {
		m := rb.bestMaker(o.Side)
		if m == nil || !crosses(o.Side, o.Type, o.Price, m.price) {
			break
		}
		if rb.selfTrade(o, m) {
			if rb.stp == STPCancelTaker {
				selfHit = true
				break
			}
			m.qty = 0
			continue
		}
		qty := min(o.Qty, m.qty)
		trades = append(trades, Trade{TakerID: o.ID, MakerID: m.id, Price: m.price, Qty: qty})
		o.Qty -= qty
		m.qty -= qty
	}
	res := Result{Status: Filled, Trades: trades, Remaining: o.Qty}
	switch {
	case o.Qty == 0:
	case selfHit, o.Type == Market, o.TIF != GTC:
		res.Status = Discarded
	default:
		res.Status = Rested
		rb.seq++
		rb.orders = append(rb.orders, &refOrder{
			id: o.ID, owner: o.Owner, side: o.Side, price: o.Price, qty: o.Qty, seq: rb.seq,
		})
	}
	return res
}

func (rb *refBook) cancel(id int64) bool {
	if o := rb.find(id); o != nil {
		o.qty = 0
		return true
	}
	return false
}

func (rb *refBook) replace(id, price, qty int64) Result {
	o := rb.find(id)
	switch {
	case o == nil:
		return Result{Status: Rejected, Reject: RejectUnknownID}
	case qty <= 0:
		return Result{Status: Rejected, Reject: RejectQty}
	case price <= 0:
		return Result{Status: Rejected, Reject: RejectPrice}
	}
	if price == o.price && qty <= o.qty {
		o.qty = qty
		return Result{Status: Rested, Remaining: qty}
	}
	o.qty = 0
	return rb.process(Order{ID: id, Owner: o.owner, Side: o.side, Type: Limit, Price: price, Qty: qty})
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

// randomOrder draws an order with the full feature mix: mostly GTC limits
// in a tight band, some IOC and FOK, some post-only, some markets, a few
// invalid prices, and owners drawn from a small pool so self-trades happen.
func randomOrder(rng *rand.Rand, id int64) Order {
	o := Order{
		ID:    id,
		Owner: int64(rng.Intn(4)), // 0 = anonymous
		Side:  Side(rng.Intn(2)),
		Type:  Limit,
		Qty:   int64(1 + rng.Intn(50)),
	}
	switch rng.Intn(20) {
	case 0, 1:
		o.TIF = IOC
	case 2:
		o.TIF = FOK
	}
	if rng.Intn(10) == 0 {
		o.PostOnly = true
	}
	switch {
	case rng.Intn(8) == 0:
		o.Type = Market
		o.PostOnly = rng.Intn(30) == 0 // occasionally invalid
	case rng.Intn(40) == 0:
		o.Price = int64(-rng.Intn(2)) // invalid price
	default:
		o.Price = int64(priceFloor + rng.Intn(priceBand))
	}
	return o
}

// TestPropertyEngineMatchesReferenceModel is the core property suite:
// 300 random scenarios x 400 operations, engine vs. reference model, with
// the STP policy rotating across scenarios.
func TestPropertyEngineMatchesReferenceModel(t *testing.T) {
	for c := 0; c < propCases; c++ {
		c := c
		stp := STPMode(c % 3)
		t.Run(fmt.Sprintf("case%03d_stp%d", c, stp), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(c)))
			eng := NewEngine(WithSelfTradePrevention(stp))
			ref := &refBook{stp: stp}
			var nextID int64
			var live []int64

			for op := 0; op < propOps; op++ {
				switch r := rng.Intn(100); {
				case r < 10 && len(live) > 0: // cancel a known id
					id := live[rng.Intn(len(live))]
					if got, want := eng.Cancel(id), ref.cancel(id); got != want {
						t.Fatalf("op %d: cancel(%d) = %v, ref %v", op, id, got, want)
					}
				case r < 18 && len(live) > 0: // replace a known id
					id := live[rng.Intn(len(live))]
					price := int64(priceFloor + rng.Intn(priceBand))
					qty := int64(1 + rng.Intn(50))
					switch rng.Intn(15) {
					case 0:
						qty = 0
					case 1:
						price = 0
					case 2:
						id = nextID + 1000 // unknown
					}
					got := eng.Replace(id, price, qty)
					got.Trades = append([]Trade(nil), got.Trades...)
					want := ref.replace(id, price, qty)
					if !sameResult(got, want) {
						t.Fatalf("op %d: replace(%d,%d,%d)\nengine: %+v\nref:    %+v", op, id, price, qty, got, want)
					}
				default:
					var id int64
					if rng.Intn(25) == 0 && len(live) > 0 { // resubmit a known id
						id = live[rng.Intn(len(live))]
					} else {
						nextID++
						id = nextID
					}
					o := randomOrder(rng, id)
					got := eng.Process(o)
					got.Trades = append([]Trade(nil), got.Trades...)
					want := ref.process(o)
					if !sameResult(got, want) {
						t.Fatalf("op %d: order %+v\nengine: %+v\nref:    %+v", op, o, got, want)
					}
					if got.Status == Rested {
						live = append(live, o.ID)
					}
				}

				// Invariant: book is never crossed.
				if bid, _, ok1 := eng.BestBid(); ok1 {
					if ask, _, ok2 := eng.BestAsk(); ok2 && bid >= ask {
						t.Fatalf("op %d: crossed book bid=%d ask=%d", op, bid, ask)
					}
				}
				// Depth must agree every 50 ops, not only at the end, so a
				// transient divergence that later heals is still caught.
				if op%50 == 49 {
					for _, side := range []Side{Buy, Sell} {
						got, want := eng.Depth(side, 10), ref.depth(side, 10)
						if len(got) == 0 && len(want) == 0 {
							continue
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("op %d: %v depth:\nengine: %v\nref:    %v", op, side, got, want)
						}
					}
				}
			}
		})
	}
}

// sameResult compares two results, treating nil and empty trade slices
// as equal.
func sameResult(a, b Result) bool {
	if a.Status != b.Status || a.Reject != b.Reject || a.Remaining != b.Remaining {
		return false
	}
	if len(a.Trades) == 0 && len(b.Trades) == 0 {
		return true
	}
	return reflect.DeepEqual(a.Trades, b.Trades)
}

// TestPropertyConservation checks that every unit of submitted quantity is
// accounted for: filled twice (maker+taker), resting, cancelled, reduced,
// or discarded. It runs the full feature mix under every STP policy and
// reads the engine's own event stream, so it also checks that the events
// tell the truth about quantities.
func TestPropertyConservation(t *testing.T) {
	for c := 0; c < 60; c++ {
		stp := STPMode(c % 3)
		rng := rand.New(rand.NewSource(int64(1000 + c)))
		eng := NewEngine(WithSelfTradePrevention(stp))
		var submitted, filled, cancelled, discarded, reduced int64
		var nextID int64
		resting := map[int64]int64{} // id -> qty the events say is on the book

		account := func() {
			for _, ev := range eng.Events() {
				switch ev.Kind {
				case EvAccepted:
					submitted += ev.Qty // includes orders re-entered by Replace
				case EvFill:
					filled += 2 * ev.Qty
					resting[ev.MakerID] -= ev.Qty
				case EvRested:
					resting[ev.OrderID] = ev.Qty
				case EvDiscarded:
					discarded += ev.Qty
				case EvCancelled:
					cancelled += ev.Qty
					delete(resting, ev.OrderID)
				case EvReduced:
					reduced += resting[ev.OrderID] - ev.Qty
					resting[ev.OrderID] = ev.Qty
				}
			}
		}

		for op := 0; op < 500; op++ {
			switch r := rng.Intn(100); {
			case r < 10 && nextID > 0:
				eng.Cancel(int64(1 + rng.Intn(int(nextID))))
				account()
			case r < 20 && nextID > 0:
				eng.Replace(int64(1+rng.Intn(int(nextID))), int64(90+rng.Intn(20)), int64(1+rng.Intn(50)))
				account()
			default:
				nextID++
				eng.Process(randomOrder(rng, nextID))
				account()
			}
		}
		// Fills consume maker quantity too; subtract what fills took from
		// the resting map by trusting the engine's depth instead.
		var onBook int64
		for _, side := range []Side{Buy, Sell} {
			for _, d := range eng.Depth(side, 1000) {
				onBook += d.Qty
			}
		}
		if submitted != filled+onBook+cancelled+discarded+reduced {
			t.Fatalf("case %d (stp %d): conservation violated: submitted=%d filled=%d onBook=%d cancelled=%d discarded=%d reduced=%d",
				c, stp, submitted, filled, onBook, cancelled, discarded, reduced)
		}
	}
}
