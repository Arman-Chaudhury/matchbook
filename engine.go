package matchbook

// Engine is a single-instrument matching engine with price-time priority.
// It is deliberately single-threaded: determinism is the property that
// makes an exchange core testable, and a single goroutine owning the book
// avoids locks entirely (shard by instrument to scale out).
type Engine struct {
	bids    *sideBook
	asks    *sideBook
	byID    map[int64]*resting
	seq     uint64
	stp     STPMode
	trade   []Trade // reused buffer to keep the hot path allocation-free
	events  []Event // reused buffer: events emitted by the last command
	evSeq   uint64  // sequence number of the last event emitted
	digest  uint64  // running hash over every event ever emitted
	scratch []*level // reused heap copy for read-only walks (FOK, Depth)
	pool    *pool   // recycled resting orders and levels, shared by both sides
}

// Option configures an Engine at construction.
type Option func(*Engine)

// WithSelfTradePrevention sets what happens when an incoming order would
// trade against a resting order with the same non-zero Owner.
func WithSelfTradePrevention(m STPMode) Option {
	return func(e *Engine) { e.stp = m }
}

func NewEngine(opts ...Option) *Engine {
	p := &pool{}
	e := &Engine{
		bids:   newSideBook(true, p),
		asks:   newSideBook(false, p),
		byID:   make(map[int64]*resting),
		digest: fnv64Offset,
		pool:   p,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// SelfTradePrevention reports the engine's configured STP policy.
func (e *Engine) SelfTradePrevention() STPMode { return e.stp }

// Apply runs one command and returns the events it produced. It is the
// single entry point a journal replays through, and it is exactly
// equivalent to calling Process, Cancel or Replace.
func (e *Engine) Apply(c Command) []Event {
	switch c.Kind {
	case CmdCancel:
		e.Cancel(c.Order.ID)
	case CmdReplace:
		e.Replace(c.Order.ID, c.Order.Price, c.Order.Qty)
	default:
		e.Process(c.Order)
	}
	return e.events
}

// Events returns the events emitted by the most recent Process, Cancel,
// Replace or Apply call, in order. The slice is only valid until the next
// call.
func (e *Engine) Events() []Event { return e.events }

// Digest is a running hash of every event the engine has emitted. Two
// engines that have consumed the same command stream have the same digest;
// replaying a journal and comparing digests proves the replay reproduced
// every output exactly.
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
// happened. The rules, in the order they are applied:
//
//   - Invalid fields, or an ID that is already resting, are rejected and
//     the book is untouched.
//   - A post-only order that would trade is rejected with RejectWouldCross.
//   - A fill-or-kill order that cannot be filled completely is accepted and
//     then discarded whole; nothing trades.
//   - Matching proceeds best price first, FIFO within a price, at the
//     maker's price. If the taker and maker share a non-zero Owner, the
//     engine's STP policy decides: cancel the incoming remainder, or cancel
//     the resting order and keep matching.
//   - Whatever is left rests if the order is a GTC limit, and is discarded
//     for market, IOC and FOK orders.
//
// Result.Trades is only valid until the next call.
func (e *Engine) Process(o Order) Result {
	e.events = e.events[:0]
	e.trade = e.trade[:0]
	return e.process(o)
}

// process is Process without resetting the output buffers, so Replace can
// prepend its own events.
func (e *Engine) process(o Order) Result {
	if r := validate(o); r != RejectNone {
		return e.reject(o.ID, r)
	}
	if _, dup := e.byID[o.ID]; dup {
		return e.reject(o.ID, RejectDuplicateID)
	}
	opposite := e.asks
	if o.Side == Sell {
		opposite = e.bids
	}
	if o.PostOnly {
		if lv := opposite.best(); lv != nil && crosses(o.Side, o.Type, o.Price, lv.price) {
			return e.reject(o.ID, RejectWouldCross)
		}
	}
	e.emit(Event{Kind: EvAccepted, OrderID: o.ID, Side: o.Side, Type: o.Type, Price: o.Price, Qty: o.Qty})

	if o.Type == Limit && o.TIF == FOK && e.available(o, opposite) < o.Qty {
		e.emit(Event{Kind: EvDiscarded, OrderID: o.ID, Qty: o.Qty})
		return Result{Status: Discarded, Trades: e.trade, Remaining: o.Qty}
	}

	selfHit := false
	for o.Qty > 0 {
		lv := opposite.best()
		if lv == nil || !crosses(o.Side, o.Type, o.Price, lv.price) {
			break
		}
		maker := lv.front()
		if o.Owner != 0 && maker.owner == o.Owner && e.stp != STPNone {
			if e.stp == STPCancelTaker {
				selfHit = true
				break
			}
			// STPCancelMaker: the resting order leaves the book, matching continues.
			e.emit(Event{Kind: EvCancelled, OrderID: maker.id, Side: maker.side, Price: maker.price, Qty: maker.qty, Reject: RejectSelfTrade})
			e.remove(maker, lv)
			continue
		}
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
			lv.unlink(maker)
		}
	}

	res := Result{Status: Filled, Trades: e.trade, Remaining: o.Qty}
	switch {
	case o.Qty == 0:
	case selfHit:
		res.Status = Discarded
		e.emit(Event{Kind: EvDiscarded, OrderID: o.ID, Qty: o.Qty, Reject: RejectSelfTrade})
	case o.Type == Market, o.TIF != GTC:
		res.Status = Discarded
		e.emit(Event{Kind: EvDiscarded, OrderID: o.ID, Qty: o.Qty})
	default:
		res.Status = Rested
		e.emit(Event{Kind: EvRested, OrderID: o.ID, Side: o.Side, Price: o.Price, Qty: o.Qty})
		e.rest(o)
	}
	return res
}

func (e *Engine) reject(id int64, r Reject) Result {
	e.emit(Event{Kind: EvRejected, OrderID: id, Reject: r})
	return Result{Status: Rejected, Reject: r, Trades: e.trade}
}

// rest places the remainder of o on its side of the book.
func (e *Engine) rest(o Order) {
	e.seq++
	r := e.pool.getOrder()
	*r = resting{id: o.ID, side: o.Side, owner: o.Owner, price: o.Price, qty: o.Qty, seq: e.seq}
	e.byID[o.ID] = r
	if o.Side == Buy {
		e.bids.add(r)
	} else {
		e.asks.add(r)
	}
}

// remove takes a resting order off the book for self-trade prevention and
// recycles it.
func (e *Engine) remove(r *resting, lv *level) {
	delete(e.byID, r.id)
	lv.qty -= r.qty
	lv.unlink(r)
}

// available returns how much of o could trade right now, applying the same
// price and STP rules the match loop would, without touching the book. It
// stops counting once o.Qty is reached. Used only for fill-or-kill, so its
// heap copy is off the common hot path.
func (e *Engine) available(o Order, opposite *sideBook) int64 {
	e.scratch = append(e.scratch[:0], opposite.heap.lvls...)
	tmp := levelHeap{lvls: e.scratch, desc: opposite.heap.desc}
	var sum int64
	for tmp.Len() > 0 && sum < o.Qty {
		lv := tmp.lvls[0]
		tmp.pop()
		if !crosses(o.Side, o.Type, o.Price, lv.price) {
			break
		}
		for m := lv.head; m != nil && sum < o.Qty; m = m.next {
			if o.Owner != 0 && m.owner == o.Owner && e.stp != STPNone {
				if e.stp == STPCancelTaker {
					return sum
				}
				continue // STPCancelMaker skips own liquidity
			}
			sum += m.qty
		}
	}
	return sum
}

// validate checks an order's fields without consulting the book.
func validate(o Order) Reject {
	switch {
	case o.Qty <= 0:
		return RejectQty
	case o.Type == Limit && o.Price <= 0:
		return RejectPrice
	case o.TIF > FOK:
		return RejectTIF
	case o.PostOnly && o.Type == Market:
		return RejectWouldCross
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
	e.cancel(r)
	return true
}

// cancel unlinks a resting order and recycles it. Callers must not use r
// afterwards.
func (e *Engine) cancel(r *resting) {
	delete(e.byID, r.id)
	r.lv.qty -= r.qty
	r.lv.unlink(r)
}

// Replace changes the price and resting quantity of a resting order. qty is
// the new resting quantity, not a total.
//
// Priority follows the usual exchange rule: shrinking an order at the same
// price keeps its place in the queue (EvReduced). Any price change, or a
// size increase, loses priority: the order is cancelled and re-entered as a
// new GTC limit order with the same ID and Owner, which may trade
// immediately. Replace of an unknown ID is rejected with RejectUnknownID.
func (e *Engine) Replace(id, price, qty int64) Result {
	e.events = e.events[:0]
	e.trade = e.trade[:0]
	r, ok := e.byID[id]
	switch {
	case !ok:
		return e.reject(id, RejectUnknownID)
	case qty <= 0:
		return e.reject(id, RejectQty)
	case price <= 0:
		return e.reject(id, RejectPrice)
	}
	if price == r.price && qty <= r.qty {
		r.lv.qty -= r.qty - qty
		r.qty = qty
		e.emit(Event{Kind: EvReduced, OrderID: id, Side: r.side, Price: price, Qty: qty})
		return Result{Status: Rested, Trades: e.trade, Remaining: qty}
	}
	e.emit(Event{Kind: EvCancelled, OrderID: id, Side: r.side, Price: r.price, Qty: r.qty, Reject: RejectReplaced})
	o := Order{ID: id, Side: r.side, Type: Limit, Price: price, Qty: qty, Owner: r.owner}
	e.cancel(r)
	return e.process(o)
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

// Depth returns up to n aggregated levels for one side, best first. It
// walks a copy of the price heap, so the live heap is untouched; each price
// appears in the heap exactly once, so no de-duplication is needed.
func (e *Engine) Depth(side Side, n int) []Depth {
	sb := e.bids
	if side == Sell {
		sb = e.asks
	}
	out := make([]Depth, 0, n)
	e.scratch = append(e.scratch[:0], sb.heap.lvls...)
	tmp := levelHeap{lvls: e.scratch, desc: sb.heap.desc}
	for len(out) < n && tmp.Len() > 0 {
		lv := tmp.lvls[0]
		tmp.pop()
		if !lv.empty() {
			out = append(out, Depth{Price: lv.price, Qty: lv.qty})
		}
	}
	return out
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
