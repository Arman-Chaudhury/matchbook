package matchbook

import (
	"reflect"
	"testing"
)

func kinds(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
	}
	return out
}

func TestIOCFillsWhatCrossesAndDropsTheRest(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))
	e.Process(limit(2, Sell, 103, 5)) // beyond the taker's limit
	res := e.Process(Order{ID: 3, Side: Buy, Type: Limit, TIF: IOC, Price: 101, Qty: 8})
	want := []Trade{{TakerID: 3, MakerID: 1, Price: 100, Qty: 5}}
	if !reflect.DeepEqual(res.Trades, want) || res.Status != Discarded || res.Remaining != 3 {
		t.Fatalf("IOC: %+v", res)
	}
	if _, _, ok := e.BestBid(); ok {
		t.Fatal("IOC remainder must not rest")
	}
	if got := kinds(e.Events()); !reflect.DeepEqual(got, []EventKind{EvAccepted, EvFill, EvDiscarded}) {
		t.Fatalf("events %v", got)
	}
	// An IOC that fully fills is just Filled.
	e.Process(limit(4, Sell, 100, 2))
	if res := e.Process(Order{ID: 5, Side: Buy, Type: Limit, TIF: IOC, Price: 100, Qty: 2}); res.Status != Filled {
		t.Fatalf("IOC full fill: %+v", res)
	}
}

func TestFOKAllOrNothing(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 4))
	e.Process(limit(2, Sell, 101, 4))
	e.Process(limit(3, Sell, 105, 100)) // outside the limit, must not count

	// 9 is available only if the taker reaches 105, which it does not.
	res := e.Process(Order{ID: 4, Side: Buy, Type: Limit, TIF: FOK, Price: 101, Qty: 9})
	if res.Status != Discarded || res.Remaining != 9 || len(res.Trades) != 0 {
		t.Fatalf("FOK should kill: %+v", res)
	}
	if got := kinds(e.Events()); !reflect.DeepEqual(got, []EventKind{EvAccepted, EvDiscarded}) {
		t.Fatalf("events %v", got)
	}
	if p, q, _ := e.BestAsk(); p != 100 || q != 4 {
		t.Fatal("killed FOK must leave the book untouched")
	}

	// Exactly 8 across two levels fills completely.
	res = e.Process(Order{ID: 5, Side: Buy, Type: Limit, TIF: FOK, Price: 101, Qty: 8})
	want := []Trade{{TakerID: 5, MakerID: 1, Price: 100, Qty: 4}, {TakerID: 5, MakerID: 2, Price: 101, Qty: 4}}
	if res.Status != Filled || !reflect.DeepEqual(res.Trades, want) {
		t.Fatalf("FOK should fill across levels: %+v", res)
	}
}

func TestFOKSkipsCancelledOrders(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))
	e.Process(limit(2, Sell, 100, 5))
	e.Cancel(1) // tombstone still physically in the level queue
	if res := e.Process(Order{ID: 3, Side: Buy, Type: Limit, TIF: FOK, Price: 100, Qty: 6}); res.Status != Discarded {
		t.Fatalf("cancelled liquidity must not count: %+v", res)
	}
	if res := e.Process(Order{ID: 4, Side: Buy, Type: Limit, TIF: FOK, Price: 100, Qty: 5}); res.Status != Filled {
		t.Fatalf("live liquidity must count: %+v", res)
	}
}

func TestPostOnly(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))

	res := e.Process(Order{ID: 2, Side: Buy, Type: Limit, PostOnly: true, Price: 100, Qty: 1})
	if res.Status != Rejected || res.Reject != RejectWouldCross {
		t.Fatalf("crossing post-only: %+v", res)
	}
	if got := kinds(e.Events()); !reflect.DeepEqual(got, []EventKind{EvRejected}) {
		t.Fatalf("events %v", got)
	}
	if p, q, _ := e.BestAsk(); p != 100 || q != 5 {
		t.Fatal("rejected post-only must not trade")
	}

	res = e.Process(Order{ID: 3, Side: Buy, Type: Limit, PostOnly: true, Price: 99, Qty: 1})
	if res.Status != Rested {
		t.Fatalf("non-crossing post-only should rest: %+v", res)
	}
	// The rejected ID is reusable, since it never rested.
	if res := e.Process(limit(2, Buy, 98, 1)); res.Status != Rested {
		t.Fatalf("id reuse after post-only reject: %+v", res)
	}
	if res := e.Process(Order{ID: 9, Side: Buy, Type: Market, PostOnly: true, Qty: 1}); res.Reject != RejectWouldCross {
		t.Fatalf("post-only market order: %+v", res)
	}
}

func TestSelfTradePreventionOff(t *testing.T) {
	e := NewEngine()
	e.Process(Order{ID: 1, Owner: 7, Side: Sell, Type: Limit, Price: 100, Qty: 5})
	res := e.Process(Order{ID: 2, Owner: 7, Side: Buy, Type: Limit, Price: 100, Qty: 5})
	if res.Status != Filled || len(res.Trades) != 1 {
		t.Fatalf("STPNone must allow self trades: %+v", res)
	}
}

func TestSelfTradePreventionCancelTaker(t *testing.T) {
	e := NewEngine(WithSelfTradePrevention(STPCancelTaker))
	e.Process(Order{ID: 1, Owner: 9, Side: Sell, Type: Limit, Price: 100, Qty: 5}) // other
	e.Process(Order{ID: 2, Owner: 7, Side: Sell, Type: Limit, Price: 101, Qty: 5}) // self
	e.Process(Order{ID: 3, Owner: 9, Side: Sell, Type: Limit, Price: 102, Qty: 5}) // other, behind self

	res := e.Process(Order{ID: 4, Owner: 7, Side: Buy, Type: Limit, Price: 102, Qty: 15})
	want := []Trade{{TakerID: 4, MakerID: 1, Price: 100, Qty: 5}}
	if !reflect.DeepEqual(res.Trades, want) || res.Status != Discarded || res.Remaining != 10 {
		t.Fatalf("cancel-taker: %+v", res)
	}
	evs := e.Events()
	if got := kinds(evs); !reflect.DeepEqual(got, []EventKind{EvAccepted, EvFill, EvDiscarded}) {
		t.Fatalf("events %v", got)
	}
	if evs[2].Reject != RejectSelfTrade {
		t.Fatalf("discard should name self-trade as the reason: %+v", evs[2])
	}
	if p, q, _ := e.BestAsk(); p != 101 || q != 5 {
		t.Fatal("own resting order must survive")
	}
	if _, _, ok := e.BestBid(); ok {
		t.Fatal("even a GTC limit remainder is discarded under cancel-taker")
	}
	// Anonymous orders are never prevented.
	if res := e.Process(Order{ID: 5, Owner: 0, Side: Buy, Type: Limit, Price: 101, Qty: 1}); res.Status != Filled {
		t.Fatalf("anonymous taker: %+v", res)
	}
}

func TestSelfTradePreventionCancelMaker(t *testing.T) {
	e := NewEngine(WithSelfTradePrevention(STPCancelMaker))
	e.Process(Order{ID: 1, Owner: 7, Side: Sell, Type: Limit, Price: 100, Qty: 5}) // self, at the front
	e.Process(Order{ID: 2, Owner: 9, Side: Sell, Type: Limit, Price: 100, Qty: 5}) // other, same level
	e.Process(Order{ID: 3, Owner: 7, Side: Sell, Type: Limit, Price: 101, Qty: 5}) // self, next level

	res := e.Process(Order{ID: 4, Owner: 7, Side: Buy, Type: Limit, Price: 101, Qty: 7})
	want := []Trade{{TakerID: 4, MakerID: 2, Price: 100, Qty: 5}}
	if !reflect.DeepEqual(res.Trades, want) || res.Status != Rested || res.Remaining != 2 {
		t.Fatalf("cancel-maker: %+v", res)
	}
	wantKinds := []EventKind{EvAccepted, EvCancelled, EvFill, EvCancelled, EvRested}
	evs := e.Events()
	if got := kinds(evs); !reflect.DeepEqual(got, wantKinds) {
		t.Fatalf("events %v, want %v", got, wantKinds)
	}
	if evs[1].OrderID != 1 || evs[1].Reject != RejectSelfTrade || evs[3].OrderID != 3 || evs[3].Reject != RejectSelfTrade {
		t.Fatalf("cancelled events should name the own orders with reason: %+v %+v", evs[1], evs[3])
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Fatal("all asks should be gone")
	}
	if p, q, _ := e.BestBid(); p != 101 || q != 2 {
		t.Fatal("remainder should rest at 101")
	}
	if e.Cancel(1) || e.Cancel(3) {
		t.Fatal("STP-cancelled orders must be gone from the index")
	}
}

func TestFOKRespectsSTP(t *testing.T) {
	// Under cancel-taker, own liquidity blocks the walk, so a FOK that would
	// need to pass it is killed rather than partially filled.
	e := NewEngine(WithSelfTradePrevention(STPCancelTaker))
	e.Process(Order{ID: 1, Owner: 9, Side: Sell, Type: Limit, Price: 100, Qty: 5})
	e.Process(Order{ID: 2, Owner: 7, Side: Sell, Type: Limit, Price: 101, Qty: 5})
	e.Process(Order{ID: 3, Owner: 9, Side: Sell, Type: Limit, Price: 102, Qty: 5})
	res := e.Process(Order{ID: 4, Owner: 7, Side: Buy, Type: Limit, TIF: FOK, Price: 102, Qty: 10})
	if res.Status != Discarded || len(res.Trades) != 0 {
		t.Fatalf("cancel-taker FOK: %+v", res)
	}
	// Under cancel-maker, own liquidity is skipped in the count and
	// cancelled during the match, so the same FOK fills from the others.
	e = NewEngine(WithSelfTradePrevention(STPCancelMaker))
	e.Process(Order{ID: 1, Owner: 9, Side: Sell, Type: Limit, Price: 100, Qty: 5})
	e.Process(Order{ID: 2, Owner: 7, Side: Sell, Type: Limit, Price: 101, Qty: 5})
	e.Process(Order{ID: 3, Owner: 9, Side: Sell, Type: Limit, Price: 102, Qty: 5})
	res = e.Process(Order{ID: 4, Owner: 7, Side: Buy, Type: Limit, TIF: FOK, Price: 102, Qty: 10})
	want := []Trade{{TakerID: 4, MakerID: 1, Price: 100, Qty: 5}, {TakerID: 4, MakerID: 3, Price: 102, Qty: 5}}
	if res.Status != Filled || !reflect.DeepEqual(res.Trades, want) {
		t.Fatalf("cancel-maker FOK: %+v", res)
	}
}

func TestReplaceReduceKeepsPriority(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 10))
	e.Process(limit(2, Sell, 100, 10))
	res := e.Replace(1, 100, 4)
	if res.Status != Rested || res.Remaining != 4 {
		t.Fatalf("reduce: %+v", res)
	}
	if got := stripSeq(e.Events()); !reflect.DeepEqual(got, []Event{{Kind: EvReduced, OrderID: 1, Side: Sell, Price: 100, Qty: 4}}) {
		t.Fatalf("events %+v", got)
	}
	if p, q, _ := e.BestAsk(); p != 100 || q != 14 {
		t.Fatalf("level qty should be 14, got %d/%d", p, q)
	}
	trades := e.Process(market(3, Buy, 6)).Trades
	want := []Trade{{TakerID: 3, MakerID: 1, Price: 100, Qty: 4}, {TakerID: 3, MakerID: 2, Price: 100, Qty: 2}}
	if !reflect.DeepEqual(trades, want) {
		t.Fatalf("order 1 should still be first: %v", trades)
	}
	// Same size at the same price is a no-op reduce and keeps priority too.
	if res := e.Replace(2, 100, 8); res.Status != Rested || e.Events()[0].Kind != EvReduced {
		t.Fatalf("same-size replace: %+v %v", res, e.Events())
	}
}

func TestReplaceIncreaseLosesPriority(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))
	e.Process(limit(2, Sell, 100, 5))
	res := e.Replace(1, 100, 6)
	if res.Status != Rested || res.Remaining != 6 {
		t.Fatalf("increase: %+v", res)
	}
	evs := e.Events()
	if got := kinds(evs); !reflect.DeepEqual(got, []EventKind{EvCancelled, EvAccepted, EvRested}) {
		t.Fatalf("events %v", got)
	}
	if evs[0].Reject != RejectReplaced || evs[0].Qty != 5 {
		t.Fatalf("cancel should be marked as a replace: %+v", evs[0])
	}
	trades := e.Process(market(3, Buy, 7)).Trades
	want := []Trade{{TakerID: 3, MakerID: 2, Price: 100, Qty: 5}, {TakerID: 3, MakerID: 1, Price: 100, Qty: 2}}
	if !reflect.DeepEqual(trades, want) {
		t.Fatalf("order 1 should now be behind order 2: %v", trades)
	}
}

func TestReplacePriceChangeMayTrade(t *testing.T) {
	e := NewEngine(WithSelfTradePrevention(STPCancelTaker))
	e.Process(Order{ID: 1, Owner: 5, Side: Buy, Type: Limit, Price: 98, Qty: 5})
	e.Process(Order{ID: 2, Owner: 6, Side: Sell, Type: Limit, Price: 100, Qty: 3})
	res := e.Replace(1, 100, 5) // now crosses the ask
	want := []Trade{{TakerID: 1, MakerID: 2, Price: 100, Qty: 3}}
	if res.Status != Rested || res.Remaining != 2 || !reflect.DeepEqual(res.Trades, want) {
		t.Fatalf("re-entered replace should trade then rest: %+v", res)
	}
	if got := kinds(e.Events()); !reflect.DeepEqual(got, []EventKind{EvCancelled, EvAccepted, EvFill, EvRested}) {
		t.Fatalf("events %v", got)
	}
	// Owner carries over: replacing into own liquidity triggers STP.
	e.Process(Order{ID: 3, Owner: 5, Side: Sell, Type: Limit, Price: 101, Qty: 1})
	res = e.Replace(1, 101, 2)
	if res.Status != Discarded || res.Reject != RejectNone || len(res.Trades) != 0 {
		t.Fatalf("replace into own order under cancel-taker: %+v", res)
	}
	if e.Cancel(1) {
		t.Fatal("order 1 should be gone after the STP discard")
	}
}

func TestReplaceRejections(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Buy, 100, 5))
	cases := []struct {
		id, price, qty int64
		want           Reject
	}{
		{42, 100, 5, RejectUnknownID},
		{1, 100, 0, RejectQty},
		{1, 0, 5, RejectPrice},
	}
	for _, c := range cases {
		res := e.Replace(c.id, c.price, c.qty)
		if res.Status != Rejected || res.Reject != c.want {
			t.Fatalf("replace(%d,%d,%d) = %+v, want reject %d", c.id, c.price, c.qty, res, c.want)
		}
		if got := kinds(e.Events()); !reflect.DeepEqual(got, []EventKind{EvRejected}) {
			t.Fatalf("events %v", got)
		}
	}
	if p, q, _ := e.BestBid(); p != 100 || q != 5 {
		t.Fatal("rejected replaces must not touch the order")
	}
	// Replace through Apply is identical.
	e2 := NewEngine()
	e2.Process(limit(1, Buy, 100, 5))
	e2.Apply(Command{Kind: CmdReplace, Order: Order{ID: 1, Price: 100, Qty: 2}})
	if got := kinds(e2.Events()); !reflect.DeepEqual(got, []EventKind{EvReduced}) {
		t.Fatalf("Apply replace events %v", got)
	}
}

func TestValidateNewFields(t *testing.T) {
	e := NewEngine()
	if res := e.Process(Order{ID: 1, Side: Buy, Type: Limit, TIF: 7, Price: 100, Qty: 1}); res.Reject != RejectTIF {
		t.Fatalf("bad TIF: %+v", res)
	}
	// Market orders ignore TIF entirely.
	e.Process(limit(2, Sell, 100, 1))
	if res := e.Process(Order{ID: 3, Side: Buy, Type: Market, TIF: FOK, Qty: 5}); res.Status != Discarded || len(res.Trades) != 1 {
		t.Fatalf("market with FOK should behave as a market order: %+v", res)
	}
	if e.SelfTradePrevention() != STPNone || NewEngine(WithSelfTradePrevention(STPCancelMaker)).SelfTradePrevention() != STPCancelMaker {
		t.Fatal("STP option not applied")
	}
	for _, tif := range []TimeInForce{GTC, IOC, FOK} {
		if tif.String() == "" {
			t.Fatal("empty TIF string")
		}
	}
}
