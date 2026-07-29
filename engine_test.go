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
	if got := e.Process(limit(1, Buy, 100, 10)); len(got) != 0 {
		t.Fatalf("expected no trades, got %v", got)
	}
	if got := e.Process(limit(2, Sell, 101, 5)); len(got) != 0 {
		t.Fatalf("expected no trades, got %v", got)
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
	trades := e.Process(limit(2, Buy, 102, 10)) // willing to pay 102, maker at 100
	want := []Trade{{TakerID: 2, MakerID: 1, Price: 100, Qty: 10}}
	if !reflect.DeepEqual(trades, want) {
		t.Fatalf("trades = %v, want %v (price improvement to taker)", trades, want)
	}
	if _, _, ok := e.BestAsk(); ok {
		t.Fatal("ask should be fully consumed")
	}
}

func TestPartialFillRestsRemainder(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 4))
	trades := e.Process(limit(2, Buy, 100, 10))
	if len(trades) != 1 || trades[0].Qty != 4 {
		t.Fatalf("trades = %v, want single fill of 4", trades)
	}
	if p, q, ok := e.BestBid(); !ok || p != 100 || q != 6 {
		t.Fatalf("remainder should rest: bid = %d/%d/%v, want 100/6", p, q, ok)
	}
}

func TestFIFOWithinLevel(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 100, 5))
	e.Process(limit(2, Sell, 100, 5))
	trades := e.Process(limit(3, Buy, 100, 7))
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
	trades := e.Process(limit(3, Buy, 102, 8))
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
	trades := e.Process(market(2, Buy, 12))
	if len(trades) != 1 || trades[0].Qty != 5 {
		t.Fatalf("trades = %v, want single fill of 5", trades)
	}
	if _, _, ok := e.BestBid(); ok {
		t.Fatal("unfilled market remainder must not rest on the book")
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
	trades := e.Process(market(3, Buy, 5))
	want := []Trade{{TakerID: 3, MakerID: 2, Price: 100, Qty: 5}}
	if !reflect.DeepEqual(trades, want) {
		t.Fatalf("trades = %v, want %v (cancelled order must not fill)", trades, want)
	}
}

func TestRejectsInvalidOrders(t *testing.T) {
	e := NewEngine()
	for _, o := range []Order{
		limit(1, Buy, 100, 0), limit(2, Buy, 100, -5),
		limit(3, Buy, 0, 5), limit(4, Buy, -1, 5),
	} {
		if got := e.Process(o); len(got) != 0 {
			t.Fatalf("invalid order %+v produced trades %v", o, got)
		}
	}
	if _, _, ok := e.BestBid(); ok {
		t.Fatal("invalid orders must not rest")
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
