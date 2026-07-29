package matchbook

// Engine is a single-instrument matching engine with price-time priority.
// It is deliberately single-threaded: determinism is the property that
// makes an exchange core testable, and a single goroutine owning the book
// avoids locks entirely (shard by instrument to scale out).
type Engine struct {
	bids  *sideBook
	asks  *sideBook
	byID  map[int64]*resting
	seq   uint64
	trade []Trade // reused buffer to keep the hot path allocation-free
}

func NewEngine() *Engine {
	return &Engine{
		bids: newSideBook(true),
		asks: newSideBook(false),
		byID: make(map[int64]*resting),
	}
}

// crosses reports whether a taker at takerPrice trades against a maker
// level at makerPrice.
func crosses(side Side, typ Type, takerPrice, makerPrice int64) bool {
	if typ == Market {
		return true
	}
	if side == Buy {
		return takerPrice >= makerPrice
	}
	return takerPrice <= makerPrice
}

// Process matches an incoming order against the book, returning the fills
// it generated. Unfilled limit quantity rests on the book; unfilled market
// quantity is discarded (immediate-or-cancel semantics).
//
// The returned slice is only valid until the next call to Process.
func (e *Engine) Process(o Order) []Trade {
	if o.Qty <= 0 || (o.Type == Limit && o.Price <= 0) {
		return nil
	}
	e.trade = e.trade[:0]

	opposite := e.asks
	if o.Side == Sell {
		opposite = e.bids
	}

	for o.Qty > 0 {
		lv := opposite.best()
		if lv == nil || !crosses(o.Side, o.Type, o.Price, lv.price) {
			break
		}
		maker := lv.front()
		qty := min(o.Qty, maker.qty)
		e.trade = append(e.trade, Trade{
			TakerID: o.ID, MakerID: maker.id, Price: maker.price, Qty: qty,
		})
		o.Qty -= qty
		maker.qty -= qty
		lv.qty -= qty
		if maker.qty == 0 {
			delete(e.byID, maker.id)
			lv.head++
		}
	}

	if o.Qty > 0 && o.Type == Limit {
		e.seq++
		r := &resting{id: o.ID, price: o.Price, qty: o.Qty, seq: e.seq}
		e.byID[o.ID] = r
		if o.Side == Buy {
			e.bids.add(r)
		} else {
			e.asks.add(r)
		}
	}
	return e.trade
}

// Cancel removes a resting order by ID. Returns false if unknown (already
// filled, cancelled, or never rested).
func (e *Engine) Cancel(id int64) bool {
	r, ok := e.byID[id]
	if !ok {
		return false
	}
	r.cancelled = true
	delete(e.byID, id)
	// Adjust the level's live quantity so depth snapshots stay accurate.
	for _, sb := range [2]*sideBook{e.bids, e.asks} {
		if lv, ok := sb.levels[r.price]; ok {
			lv.qty -= r.qty
		}
	}
	r.qty = 0
	return true
}

// BestBid and BestAsk return the top of book (price, qty, ok).
func (e *Engine) BestBid() (int64, int64, bool) { return topOf(e.bids) }
func (e *Engine) BestAsk() (int64, int64, bool) { return topOf(e.asks) }

func topOf(sb *sideBook) (int64, int64, bool) {
	lv := sb.best()
	if lv == nil {
		return 0, 0, false
	}
	return lv.price, lv.qty, true
}

// Depth returns up to n aggregated levels for one side, best first.
func (e *Engine) Depth(side Side, n int) []Depth {
	sb := e.bids
	if side == Sell {
		sb = e.asks
	}
	out := make([]Depth, 0, n)
	seen := make(map[int64]bool)
	// Walk a copy of the heap so we don't disturb ordering.
	tmp := priceHeap{prices: append([]int64(nil), sb.heap.prices...), desc: sb.heap.desc}
	for len(out) < n && tmp.Len() > 0 {
		price := tmp.prices[0]
		(&tmp).Pop2()
		if seen[price] {
			continue
		}
		seen[price] = true
		if lv, ok := sb.levels[price]; ok && !lv.empty() {
			out = append(out, Depth{Price: price, Qty: lv.qty})
		}
	}
	return out
}

// Pop2 pops the heap root (small helper avoiding interface boxing in Depth).
func (h *priceHeap) Pop2() {
	n := h.Len()
	h.Swap(0, n-1)
	h.prices = h.prices[:n-1]
	if n > 1 {
		siftDown(h, 0)
	}
}

func siftDown(h *priceHeap, i int) {
	n := h.Len()
	for {
		l, r := 2*i+1, 2*i+2
		smallest := i
		if l < n && h.Less(l, smallest) {
			smallest = l
		}
		if r < n && h.Less(r, smallest) {
			smallest = r
		}
		if smallest == i {
			return
		}
		h.Swap(i, smallest)
		i = smallest
	}
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
