// Package replay drives matchbook engines with a real NASDAQ ITCH 5.0 feed
// and checks the engine against what the exchange actually did.
//
// Each symbol gets its own engine. Next to it the replay keeps a shadow
// book built only from the feed's messages, with no matching logic, as an
// independent record of what the displayed book was.
//
// The checks, in order of strength:
//
//   - Priority. When NASDAQ reports that resting order X executed at its
//     own price, X must be the order the engine would match first: the
//     oldest order at the best price. The trade is then replayed through
//     the engine's real matching path with an immediate-or-cancel order
//     from the other side, and the fill must be exactly X, at X's price,
//     for the reported size.
//   - Book. After every message the engine's best bid and ask, with their
//     sizes, must equal the shadow book's; at intervals and at the end the
//     full depth must match level for level.
//   - Explained disagreements. NASDAQ ranks orders at a price by the order
//     reference number, which it assigns on receipt, while the engine ranks
//     them by when they reach it. The two differ only for an order NASDAQ
//     received earlier but published later. When the engine had a different
//     order first, the disagreement counts as explained only if the executed
//     order has the lowest reference number at its price and was published
//     after the engine's front order. Anything else is unexplained.
//   - Mechanics. Partial cancels are applied as in-place reductions, which
//     must keep priority; deletes must find the order; adds must rest.
//
// Executions that do not come from continuous matching, such as the
// opening and closing crosses (reported as executions with a price), are
// applied directly and counted separately. Hidden-order trades never touch
// the displayed book and are only counted.
package replay

import (
	"fmt"
	"sort"

	"github.com/Arman-Chaudhury/matchbook"
	"github.com/Arman-Chaudhury/matchbook/internal/itch"
)

// Stats counts what happened to one symbol.
type Stats struct {
	Messages int

	Adds, Cancels, Deletes, Replaces int
	AddsWouldCross                   int // add would have traded: not placed in the engine

	Executions       int // 'E' messages
	ExecMatched      int // replayed through matching, exact fill
	ExecNotFront     int // X was at the best price but not first in line
	ExecReceiptOrder int // ...and that is explained by receipt-order priority
	ExecNotBestPrice int // X was not at the best price at all
	ExecWrongFill    int // X was first, but matching produced a different fill
	ExecWithPrice    int // 'C' messages, applied directly
	ExecOnUnplaced   int // execution of an order the engine never held
	HiddenTrades     int // 'P' messages
	CrossTrades      int // 'Q' messages
	TradingActions   int

	TopChecks     int
	TopMismatches int
	DepthChecks   int
	DepthMismatch int
	UnknownRef    int // message named an order the replay never saw
}

// Mismatch records one disagreement for the report.
type Mismatch struct {
	Kind      string
	Timestamp uint64
	Detail    string
}

type shadowOrder struct {
	side   matchbook.Side
	price  int64
	qty    int64
	placed bool   // false if the engine refused it (AddsWouldCross)
	added  uint64 // feed timestamp of the add, for diagnosing priority
}

// Symbol replays one instrument.
type Symbol struct {
	Name   string
	Engine *matchbook.Engine
	Stats  Stats

	orders     map[uint64]*shadowOrder
	bids, asks *shadowSide
	nextAggID  int64
	mismatches []Mismatch
	maxKeep    int
	depthEvery int
}

// Options tune checking cost. The zero value checks depth every 10,000
// messages and keeps the first 20 mismatches of each kind.
type Options struct {
	DepthEvery  int
	KeepPerKind int
}

func NewSymbol(name string, opt Options) *Symbol {
	if opt.DepthEvery <= 0 {
		opt.DepthEvery = 10_000
	}
	if opt.KeepPerKind <= 0 {
		opt.KeepPerKind = 20
	}
	return &Symbol{
		Name:       name,
		Engine:     matchbook.NewEngine(),
		orders:     map[uint64]*shadowOrder{},
		bids:       newShadowSide(true),
		asks:       newShadowSide(false),
		nextAggID:  -1,
		maxKeep:    opt.KeepPerKind,
		depthEvery: opt.DepthEvery,
	}
}

// Mismatches returns the recorded disagreements, oldest first.
func (s *Symbol) Mismatches() []Mismatch { return s.mismatches }

func (s *Symbol) record(kind string, ts uint64, format string, args ...any) {
	n := 0
	for _, m := range s.mismatches {
		if m.Kind == kind {
			n++
		}
	}
	if n < s.maxKeep {
		s.mismatches = append(s.mismatches, Mismatch{Kind: kind, Timestamp: ts, Detail: fmt.Sprintf(format, args...)})
	}
}

func (s *Symbol) side(sd matchbook.Side) *shadowSide {
	if sd == matchbook.Buy {
		return s.bids
	}
	return s.asks
}

// Apply processes one message for this symbol.
func (s *Symbol) Apply(m itch.Msg) {
	s.Stats.Messages++
	ts := m.Timestamp()
	switch m.Type() {
	case itch.TypeAddOrder, itch.TypeAddOrderMPID:
		a := m.AddOrder()
		s.Stats.Adds++
		side := matchbook.Sell
		if a.Buy {
			side = matchbook.Buy
		}
		s.add(ts, a.Ref, side, int64(a.Price), int64(a.Shares))

	case itch.TypeOrderExecuted:
		e := m.Executed()
		s.Stats.Executions++
		s.execute(ts, e.Ref, int64(e.Shares))

	case itch.TypeExecutedWithPrice:
		e := m.ExecutedWithPrice()
		s.Stats.ExecWithPrice++
		s.reduce(ts, e.Ref, int64(e.Shares))

	case itch.TypeOrderCancel:
		c := m.OrderCancel()
		s.Stats.Cancels++
		s.reduce(ts, c.Ref, int64(c.Shares))

	case itch.TypeOrderDelete:
		s.Stats.Deletes++
		ref := m.OrderDelete().Ref
		if o, ok := s.orders[ref]; ok {
			s.reduce(ts, ref, o.qty)
		} else {
			s.Stats.UnknownRef++
			s.record("unknown-ref", ts, "delete of unknown order %d", ref)
		}

	case itch.TypeOrderReplace:
		u := m.OrderReplace()
		s.Stats.Replaces++
		o, ok := s.orders[u.Orig]
		if !ok {
			s.Stats.UnknownRef++
			s.record("unknown-ref", ts, "replace of unknown order %d", u.Orig)
			break
		}
		side := o.side
		s.reduce(ts, u.Orig, o.qty)
		s.add(ts, u.New, side, int64(u.Price), int64(u.Shares))

	case itch.TypeTrade:
		s.Stats.HiddenTrades++
	case itch.TypeCrossTrade:
		s.Stats.CrossTrades++
	case itch.TypeTradingAction:
		s.Stats.TradingActions++
	default:
		return
	}

	s.checkTop(ts)
	if s.Stats.Messages%s.depthEvery == 0 {
		s.CheckDepth(ts)
	}
}

func (s *Symbol) add(ts uint64, ref uint64, side matchbook.Side, price, qty int64) {
	o := &shadowOrder{side: side, price: price, qty: qty, placed: true, added: ts}
	s.orders[ref] = o
	res := s.Engine.Process(matchbook.Order{
		ID: int64(ref), Side: side, Type: matchbook.Limit, PostOnly: true, Price: price, Qty: qty,
	})
	switch {
	case res.Status == matchbook.Rested:
		// The shadow book holds exactly the orders the engine accepted, so
		// a refused order is counted once instead of skewing every check.
		s.side(side).change(price, qty)
	case res.Reject == matchbook.RejectWouldCross:
		o.placed = false
		s.Stats.AddsWouldCross++
		s.record("add-would-cross", ts, "order %d %v %d@%s would trade on arrival", ref, side, qty, px(price))
	default:
		s.record("add-refused", ts, "order %d: engine returned %v reject %d", ref, res.Status, res.Reject)
		o.placed = false
	}
}

// execute handles an execution at the resting order's own price.
func (s *Symbol) execute(ts uint64, ref uint64, shares int64) {
	o, ok := s.orders[ref]
	if !ok {
		s.Stats.UnknownRef++
		s.record("unknown-ref", ts, "execution of unknown order %d", ref)
		return
	}
	if !o.placed {
		s.Stats.ExecOnUnplaced++
		s.reduce(ts, ref, shares)
		return
	}
	id, bestPx, _, _ := s.Engine.BestOrder(o.side)
	switch {
	case bestPx != o.price:
		s.Stats.ExecNotBestPrice++
		s.record("exec-not-best-price", ts, "order %d at %s executed while engine best was %s", ref, px(o.price), px(bestPx))
		s.reduce(ts, ref, shares)
		return
	case id != int64(ref):
		s.Stats.ExecNotFront++
		front := s.orders[uint64(id)]
		if s.lowestRefAtLevel(ref, o) && front.added < o.added {
			s.Stats.ExecReceiptOrder++
			s.record("exec-receipt-order", ts, "order %d (published %s) executed before engine-first order %d (published %s, higher ref)",
				ref, clock(o.added), id, clock(front.added))
		} else {
			s.record("exec-not-front", ts, "order %d (published %s) executed at %s but engine had order %d (published %s) first",
				ref, clock(o.added), px(o.price), id, clock(front.added))
		}
		s.reduce(ts, ref, shares)
		return
	}

	// Replay the trade through matching.
	agg := matchbook.Order{
		ID: s.nextAggID, Side: o.side.Opposite(), Type: matchbook.Limit,
		TIF: matchbook.IOC, Price: o.price, Qty: shares,
	}
	s.nextAggID--
	res := s.Engine.Process(agg)
	want := matchbook.Trade{TakerID: agg.ID, MakerID: int64(ref), Price: o.price, Qty: shares}
	if len(res.Trades) != 1 || res.Trades[0] != want || res.Status != matchbook.Filled {
		s.Stats.ExecWrongFill++
		s.record("exec-wrong-fill", ts, "order %d: want %+v, engine filled %+v", ref, want, res.Trades)
	} else {
		s.Stats.ExecMatched++
	}
	s.settle(ref, o, shares)
}

// lowestRefAtLevel reports whether ref is the lowest reference number among
// the engine-held orders resting at o's side and price. It scans every
// order, which is fine because it only runs on disagreements.
func (s *Symbol) lowestRefAtLevel(ref uint64, o *shadowOrder) bool {
	for r, x := range s.orders {
		if x.placed && x.side == o.side && x.price == o.price && r < ref {
			return false
		}
	}
	return true
}

// Unexplained counts everything that neither the engine's rules nor
// NASDAQ's receipt-order priority accounts for.
func (st Stats) Unexplained() int {
	return st.ExecNotFront - st.ExecReceiptOrder + st.ExecNotBestPrice + st.ExecWrongFill +
		st.TopMismatches + st.DepthMismatch + st.UnknownRef
}

// reduce removes shares from an order without matching: cancels, deletes,
// cross executions, and executions the engine disagreed about.
func (s *Symbol) reduce(ts uint64, ref uint64, shares int64) {
	o, ok := s.orders[ref]
	if !ok {
		s.Stats.UnknownRef++
		s.record("unknown-ref", ts, "reduce of unknown order %d", ref)
		return
	}
	if shares > o.qty {
		s.record("over-reduce", ts, "order %d: removing %d of %d", ref, shares, o.qty)
		shares = o.qty
	}
	if o.placed {
		left := o.qty - shares
		if left == 0 {
			if !s.Engine.Cancel(int64(ref)) {
				s.record("engine-lost-order", ts, "cancel of order %d failed", ref)
			}
		} else {
			s.Engine.Replace(int64(ref), o.price, left)
			if ev := s.Engine.Events(); len(ev) != 1 || ev[0].Kind != matchbook.EvReduced {
				s.record("reduce-not-in-place", ts, "order %d: reduce to %d gave %v", ref, left, ev)
			}
		}
	}
	s.settle(ref, o, shares)
}

// settle updates the shadow book after shares left order ref, by whatever
// route. The engine has already been updated.
func (s *Symbol) settle(ref uint64, o *shadowOrder, shares int64) {
	if o.placed {
		s.side(o.side).change(o.price, -shares)
	}
	o.qty -= shares
	if o.qty == 0 {
		delete(s.orders, ref)
	}
}

func (s *Symbol) checkTop(ts uint64) {
	s.Stats.TopChecks++
	for _, sd := range []matchbook.Side{matchbook.Buy, matchbook.Sell} {
		var ep, eq int64
		var eok bool
		if sd == matchbook.Buy {
			ep, eq, eok = s.Engine.BestBid()
		} else {
			ep, eq, eok = s.Engine.BestAsk()
		}
		sp, sq, sok := s.side(sd).best()
		if eok != sok || (eok && (ep != sp || eq != sq)) {
			s.Stats.TopMismatches++
			s.record("top-of-book", ts, "%v: engine %s x %d (%v), feed %s x %d (%v)", sd, px(ep), eq, eok, px(sp), sq, sok)
		}
	}
}

// CheckDepth compares every level on both sides.
func (s *Symbol) CheckDepth(ts uint64) {
	s.Stats.DepthChecks++
	for _, sd := range []matchbook.Side{matchbook.Buy, matchbook.Sell} {
		want := s.side(sd).depth()
		// One more level than the feed has is enough to expose an extra
		// level in the engine; Depth sizes its result by this count.
		got := s.Engine.Depth(sd, len(want)+1)
		if len(got) != len(want) {
			s.Stats.DepthMismatch++
			s.record("depth", ts, "%v: engine %d levels, feed %d", sd, len(got), len(want))
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				s.Stats.DepthMismatch++
				s.record("depth", ts, "%v level %d: engine %s x %d, feed %s x %d", sd, i, px(got[i].Price), got[i].Qty, px(want[i].Price), want[i].Qty)
				break
			}
		}
	}
}

// RestingOrders is the number of orders in the shadow book.
func (s *Symbol) RestingOrders() int { return len(s.orders) }

// px formats a four-decimal ITCH price.
func px(p int64) string { return fmt.Sprintf("%d.%04d", p/10000, p%10000) }

// shadowSide aggregates quantity by price with a lazily maintained best.
type shadowSide struct {
	levels map[int64]int64
	desc   bool
	bestPx int64
	valid  bool
}

func newShadowSide(desc bool) *shadowSide { return &shadowSide{levels: map[int64]int64{}, desc: desc} }

func (s *shadowSide) better(a, b int64) bool {
	if s.desc {
		return a > b
	}
	return a < b
}

func (s *shadowSide) change(price, delta int64) {
	q := s.levels[price] + delta
	if q == 0 {
		delete(s.levels, price)
		if s.valid && price == s.bestPx {
			s.valid = false
		}
		return
	}
	s.levels[price] = q
	if s.valid && s.better(price, s.bestPx) {
		s.bestPx = price
	}
}

func (s *shadowSide) best() (int64, int64, bool) {
	if len(s.levels) == 0 {
		return 0, 0, false
	}
	if !s.valid {
		first := true
		for p := range s.levels {
			if first || s.better(p, s.bestPx) {
				s.bestPx, first = p, false
			}
		}
		s.valid = true
	}
	return s.bestPx, s.levels[s.bestPx], true
}

func (s *shadowSide) depth() []matchbook.Depth {
	out := make([]matchbook.Depth, 0, len(s.levels))
	for p, q := range s.levels {
		out = append(out, matchbook.Depth{Price: p, Qty: q})
	}
	sort.Slice(out, func(i, j int) bool { return s.better(out[i].Price, out[j].Price) })
	return out
}

// clock formats nanoseconds since midnight as HH:MM:SS.mmm.
func clock(ns uint64) string {
	ms := ns / 1_000_000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}
