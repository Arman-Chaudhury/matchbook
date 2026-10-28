package matchbook

import (
	"reflect"
	"testing"
)

func limit(id int64, side Side, price, qty int64) Order {
	return Order{ID: id, Side: side, Type: Limit, Price: price, Qty: qty}
}

func market(id int64, side Side, qty int64) Order {
	return Order{ID: id, Side: side, Type: Market, Qty: qty}
}

func TestRestingOrderNoCross(t *testing.T) {
	e := NewEngine()
	if got := e.Process(limit(1, Buy, 100, 10)); got.Status != Rested || len(got.Trades) != 0 || got.Remaining != 10 {
		t.Fatalf("expected rested with no trades, got %+v", got)
	}
	if got := e.Process(limit(2, Sell, 101, 5)); got.Status != Rested || len(got.Trades) != 0 {
		t.Fatalf("expected rested with no trades, got %+v", got)
	}
	if p, q, ok := e.BestBid(); !ok || p != 100 || q != 10 {
		t.Fatalf("best bid = %d/%d/%v, want 100/10", p, q, ok)
	}
	if p, q, ok := e.BestAsk(); !ok || p != 101 || q != 5 {
		t.Fatalf("best ask = %d/%d/%v, want 101/5", p, q, ok)
	}
}

func TestFullFillAtMakerPrice(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 10))
	res := e.Process(limit(2, Buy, 102, 10)) // willing to pay 102, maker at 100
	want := []Trade{{TakerID: 2, MakerID: 1, Price: 100, Qty: 10}}
	if !reflect.DeepEqual(res.Trades, want) {
		t.Fatalf("trades = %v, want %v (price improvement to taker)", res.Trades, want)
	}
	if res.Status != Filled || res.Remaining != 0 {
		t.Fatalf("status = %v remaining = %d, want filled/0", res.Status, res.Remaining)
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Fatal("ask should be fully consumed")
	}
}

func TestPartialFillRestsRemainder(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 4))
	res := e.Process(limit(2, Buy, 100, 10))
	if len(res.Trades) != 1 || res.Trades[0].Qty != 4 {
		t.Fatalf("trades = %v, want single fill of 4", res.Trades)
	}
	if res.Status != Rested || res.Remaining != 6 {
		t.Fatalf("status = %v remaining = %d, want rested/6", res.Status, res.Remaining)
	}
	if p, q, ok := e.BestBid(); !ok || p != 100 || q != 6 {
		t.Fatalf("remainder should rest: bid = %d/%d/%v, want 100/6", p, q, ok)
	}
}

func TestFIFOWithinLevel(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))
	e.Process(limit(2, Sell, 100, 5))
	trades := e.Process(limit(3, Buy, 100, 7)).Trades
	want := []Trade{
		{TakerID: 3, MakerID: 1, Price: 100, Qty: 5},
		{TakerID: 3, MakerID: 2, Price: 100, Qty: 2},
	}
	if !reflect.DeepEqual(trades, want) {
		t.Fatalf("trades = %v, want %v (earlier order fills first)", trades, want)
	}
}

func TestPricePriorityAcrossLevels(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 102, 5))
	e.Process(limit(2, Sell, 100, 5)) // better ask arrives later
	trades := e.Process(limit(3, Buy, 102, 8)).Trades
	want := []Trade{
		{TakerID: 3, MakerID: 2, Price: 100, Qty: 5},
		{TakerID: 3, MakerID: 1, Price: 102, Qty: 3},
	}
	if !reflect.DeepEqual(trades, want) {
		t.Fatalf("trades = %v, want %v (best price first)", trades, want)
	}
}

func TestMarketOrderIOC(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))
	res := e.Process(market(2, Buy, 12))
	if len(res.Trades) != 1 || res.Trades[0].Qty != 5 {
		t.Fatalf("trades = %v, want single fill of 5", res.Trades)
	}
	if res.Status != Discarded || res.Remaining != 7 {
		t.Fatalf("status = %v remaining = %d, want discarded/7", res.Status, res.Remaining)
	}
	if _, _, ok := e.BestBid(); ok {
		t.Fatal("unfilled market remainder must not rest on the book")
	}
	if res := e.Process(market(3, Buy, 1)); res.Status != Discarded || res.Remaining != 1 {
		t.Fatalf("market order into an empty book: %+v, want discarded/1", res)
	}
}

func TestCancel(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))
	e.Process(limit(2, Sell, 100, 7))
	if !e.Cancel(1) {
		t.Fatal("cancel of resting order should succeed")
	}
	if e.Cancel(1) {
		t.Fatal("double cancel should fail")
	}
	if e.Cancel(99) {
		t.Fatal("cancel of unknown id should fail")
	}
	trades := e.Process(market(3, Buy, 5)).Trades
	want := []Trade{{TakerID: 3, MakerID: 2, Price: 100, Qty: 5}}
	if !reflect.DeepEqual(trades, want) {
		t.Fatalf("trades = %v, want %v (cancelled order must not fill)", trades, want)
	}
}

func TestRejectsInvalidOrders(t *testing.T) {
	e := NewEngine()
	for _, tc := range []struct {
		o    Order
		want Reject
	}{
		{limit(1, Buy, 100, 0), RejectQty}, {limit(2, Buy, 100, -5), RejectQty},
		{limit(3, Buy, 0, 5), RejectPrice}, {limit(4, Buy, -1, 5), RejectPrice},
		{market(5, Buy, 0), RejectQty},
	} {
		got := e.Process(tc.o)
		if got.Status != Rejected || got.Reject != tc.want || len(got.Trades) != 0 {
			t.Fatalf("invalid order %+v: got %+v, want rejected/%d", tc.o, got, tc.want)
		}
	}
	if _, _, ok := e.BestBid(); ok {
		t.Fatal("invalid orders must not rest")
	}
}

func TestRejectsDuplicateID(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))
	got := e.Process(limit(1, Sell, 101, 7))
	if got.Status != Rejected || got.Reject != RejectDuplicateID {
		t.Fatalf("duplicate id: got %+v, want rejected/duplicate", got)
	}
	if d := e.Depth(Sell, 5); !reflect.DeepEqual(d, []Depth{{Price: 100, Qty: 5}}) {
		t.Fatalf("duplicate must not touch the book, depth = %v", d)
	}
	if !e.Cancel(1) {
		t.Fatal("original order must still be cancellable")
	}
	if d := e.Depth(Sell, 5); len(d) != 0 {
		t.Fatalf("book should be empty after cancel, depth = %v", d)
	}
	// Once the original is gone the ID may be reused.
	if got := e.Process(limit(1, Buy, 99, 1)); got.Status != Rested {
		t.Fatalf("reused id after cancel: got %+v, want rested", got)
	}
	// Same for an ID that was fully filled.
	e.Process(limit(2, Sell, 99, 1))
	if got := e.Process(limit(2, Sell, 98, 1)); got.Status != Rested {
		t.Fatalf("reused id after fill: got %+v, want rested", got)
	}
}

func TestCancelAdjustsOnlyOwnSide(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Buy, 100, 5))
	e.Process(limit(2, Sell, 101, 3))
	e.Cancel(1)
	if p, q, ok := e.BestAsk(); !ok || p != 101 || q != 3 {
		t.Fatalf("ask side must be untouched: %d/%d/%v", p, q, ok)
	}
	if _, _, ok := e.BestBid(); ok {
		t.Fatal("bid should be gone")
	}
}

// TestLevelCompaction pins the memory fix: a busy price level must not
// retain every dead order it ever held.
func TestLevelCompaction(t *testing.T) {
	e := NewEngine()
	const n = 10_000
	for i := int64(1); i <= n; i++ {
		e.Process(limit(i, Buy, 100, 1))
		e.Process(limit(i+n, Sell, 100, 1))
	}
	lv := e.bids.levels[100]
	if lv == nil {
		t.Fatal("level 100 should still exist until pruned")
	}
	if lv.count != 0 || lv.head != nil || lv.qty != 0 {
		t.Fatalf("level retained %d orders, qty %d, after every order filled", lv.count, lv.qty)
	}
	// Compaction must not break FIFO or cancels of orders still queued.
	e.Process(limit(n*3+1, Buy, 100, 1))
	e.Process(limit(n*3+2, Buy, 100, 1))
	if !e.Cancel(n*3 + 1) {
		t.Fatal("cancel after compaction should succeed")
	}
	got := e.Process(limit(n*3+3, Sell, 100, 1)).Trades
	want := []Trade{{TakerID: n*3 + 3, MakerID: n*3 + 2, Price: 100, Qty: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trades = %v, want %v", got, want)
	}
}

func TestBestOrder(t *testing.T) {
	e := NewEngine()
	if _, _, _, ok := e.BestOrder(Buy); ok {
		t.Fatal("empty book has no best order")
	}
	e.Process(limit(1, Sell, 101, 5))
	e.Process(limit(2, Sell, 100, 3)) // better price, later
	e.Process(limit(3, Sell, 100, 4)) // same price, behind 2
	e.Process(limit(4, Buy, 99, 6))
	if id, p, q, ok := e.BestOrder(Sell); !ok || id != 2 || p != 100 || q != 3 {
		t.Fatalf("best ask order %d %d/%d %v, want 2 at 100/3", id, p, q, ok)
	}
	if id, _, _, _ := e.BestOrder(Buy); id != 4 {
		t.Fatalf("best bid order %d, want 4", id)
	}
	e.Cancel(2)
	if id, _, _, _ := e.BestOrder(Sell); id != 3 {
		t.Fatalf("after cancel, best ask order %d, want 3", id)
	}
	// It must agree with what matching actually does.
	if tr := e.Process(market(5, Buy, 1)).Trades; tr[0].MakerID != 3 {
		t.Fatalf("matching took %d, BestOrder said 3", tr[0].MakerID)
	}
}

func TestDepthAggregation(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Buy, 100, 5))
	e.Process(limit(2, Buy, 100, 3))
	e.Process(limit(3, Buy, 99, 7))
	got := e.Depth(Buy, 5)
	want := []Depth{{Price: 100, Qty: 8}, {Price: 99, Qty: 7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("depth = %v, want %v", got, want)
	}
}
