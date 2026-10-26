package matchbook

// Engine is a single-instrument matching engine with price-time priority.
// It is deliberately single-threaded: determinism is the property that
// makes an exchange core testable, and a single goroutine owning the book
// avoids locks entirely (shard by instrument to scale out).
type Engine struct {
	bids   *sideBook
	asks   *sideBook
	byID   map[int64]*resting
	seq    uint64
	trade  []Trade // reused buffer to keep the hot path allocation-free
	events []Event // reused buffer: events emitted by the last command
	evSeq  uint64  // sequence number of the last event emitted
	digest uint64  // FNV-1a over every event ever emitted
}

func NewEngine() *Engine {
	return &Engine{
		bids:   newSideBook(true),
		asks:   newSideBook(false),
		byID:   make(map[int64]*resting),
		digest: fnv64Offset,
	}
}

// Apply runs one command and returns the events it produced. It is the
// single entry point a journal replays through, and it is exactly
// equivalent to calling Process or Cancel.
func (e *Engine) Apply(c Command) []Event {
	if c.Kind == CmdCancel {
		e.Cancel(c.Order.ID)
	} else {
		e.Process(c.Order)
	}
	return e.events
}

// Events returns the events emitted by the most recent Process, Cancel or
// Apply call, in order. The slice is only valid until the next call.
func (e *Engine) Events() []Event { return e.events }

// Digest is a running FNV-1a hash of every event the engine has emitted.
// Two engines that have consumed the same command stream have the same
// digest; replaying a journal and comparing digests proves the replay
// reproduced every output exactly.
func (e *Engine) Digest() uint64 { return e.digest }

// EventSeq is the sequence number of the last event emitted.
func (e *Engine) EventSeq() uint64 { return e.evSeq }

func (e *Engine) emit(ev Event) {
	e.evSeq++
	ev.Seq = e.evSeq
	e.events = append(e.events, ev)
	e.digest = ev.digest(e.digest)
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

// Process matches an incoming order against the book and reports what
// happened. Unfilled limit quantity rests on the book; unfilled market
// quantity is discarded (immediate-or-cancel semantics). An order whose ID
// is already resting is rejected and the book is left untouched.
//
// Result.Trades is only valid until the next call to Process.
func (e *Engine) Process(o Order) Result {
	e.events = e.events[:0]
	e.trade = e.trade[:0]
	if r := validate(o); r != RejectNone {
		e.emit(Event{Kind: EvRejected, OrderID: o.ID, Reject: r})
		return Result{Status: Rejected, Reject: r}
	}
	if _, dup := e.byID[o.ID]; dup {
		e.emit(Event{Kind: EvRejected, OrderID: o.ID, Reject: RejectDuplicateID})
		return Result{Status: Rejected, Reject: RejectDuplicateID}
	}
	e.emit(Event{Kind: EvAccepted, OrderID: o.ID, Side: o.Side, Type: o.Type, Price: o.Price, Qty: o.Qty})

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
		e.emit(Event{Kind: EvFill, OrderID: o.ID, MakerID: maker.id, Price: maker.price, Qty: qty})
		o.Qty -= qty
		maker.qty -= qty
		lv.qty -= qty
		if maker.qty == 0 {
			delete(e.byID, maker.id)
			lv.head++
		}
	}

	res := Result{Status: Filled, Trades: e.trade, Remaining: o.Qty}
	switch {
	case o.Qty == 0:
	case o.Type == Market:
		res.Status = Discarded
		e.emit(Event{Kind: EvDiscarded, OrderID: o.ID, Qty: o.Qty})
	default:
		res.Status = Rested
		e.emit(Event{Kind: EvRested, OrderID: o.ID, Side: o.Side, Price: o.Price, Qty: o.Qty})
		e.seq++
		r := &resting{id: o.ID, side: o.Side, price: o.Price, qty: o.Qty, seq: e.seq}
		e.byID[o.ID] = r
		if o.Side == Buy {
			e.bids.add(r)
		} else {
			e.asks.add(r)
		}
	}
	return res
}

// validate checks an order's fields without consulting the book.
func validate(o Order) Reject {
	switch {
	case o.Qty <= 0:
		return RejectQty
	case o.Type == Limit && o.Price <= 0:
		return RejectPrice
	}
	return RejectNone
}

// Cancel removes a resting order by ID. Returns false if unknown (already
// filled, cancelled, or never rested).
func (e *Engine) Cancel(id int64) bool {
	e.events = e.events[:0]
	r, ok := e.byID[id]
	if !ok {
		e.emit(Event{Kind: EvCancelRejected, OrderID: id})
		return false
	}
	e.emit(Event{Kind: EvCancelled, OrderID: id, Side: r.side, Price: r.price, Qty: r.qty})
	r.cancelled = true
	delete(e.byID, id)
	// Adjust the level's live quantity so depth snapshots stay accurate.
	sb := e.bids
	if r.side == Sell {
		sb = e.asks
	}
	sb.levels[r.price].qty -= r.qty
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
