package matchbook

// resting is an order sitting on the book. It is a node in its level's
// intrusive doubly linked list, so it can be unlinked from anywhere in the
// queue in O(1) without a tombstone.
type resting struct {
	id         int64
	owner      int64
	side       Side
	price      int64
	qty        int64
	seq        uint64 // arrival sequence: FIFO within a price level
	lv         *level // the level this order sits in, so cancel needs no lookup
	prev, next *resting
}

// pool recycles resting orders and price levels so that steady-state
// trading does not allocate. An order goes back to the pool the moment it
// is unlinked from its level, by a fill, a cancel or self-trade prevention;
// nothing points at it after that. A level goes back when its side book
// prunes it as empty.
type pool struct {
	orders []*resting
	levels []*level
}

func (p *pool) getOrder() *resting {
	if n := len(p.orders); n > 0 {
		r := p.orders[n-1]
		p.orders = p.orders[:n-1]
		return r
	}
	return new(resting)
}

func (p *pool) putOrder(r *resting) {
	*r = resting{}
	p.orders = append(p.orders, r)
}

func (p *pool) getLevel(price int64) *level {
	if n := len(p.levels); n > 0 {
		lv := p.levels[n-1]
		p.levels = p.levels[:n-1]
		lv.price = price
		return lv
	}
	return &level{price: price, pool: p}
}

func (p *pool) putLevel(lv *level) {
	lv.head, lv.tail, lv.count, lv.qty = nil, nil, 0, 0
	p.levels = append(p.levels, lv)
}

// level is the FIFO queue of resting orders at one price, kept as an
// intrusive doubly linked list: push appends at the tail, matching takes
// from the head, and a cancel unlinks its order wherever it sits. Every
// operation is O(1) and the queue only ever holds live orders, so there is
// nothing to compact or sweep.
type level struct {
	price      int64
	head, tail *resting
	count      int   // live orders in the queue
	qty        int64 // live quantity in the queue
	pool       *pool
}

func (l *level) push(r *resting) {
	r.prev, r.next = l.tail, nil
	if l.tail == nil {
		l.head = r
	} else {
		l.tail.next = r
	}
	l.tail = r
	l.count++
	l.qty += r.qty
}

// unlink removes r from the queue and recycles it. It does not touch qty;
// the caller has already settled r's quantity against the level.
func (l *level) unlink(r *resting) {
	if r.prev == nil {
		l.head = r.next
	} else {
		r.prev.next = r.next
	}
	if r.next == nil {
		l.tail = r.prev
	} else {
		r.next.prev = r.prev
	}
	l.count--
	l.pool.putOrder(r)
}

// front returns the first order in the queue, or nil.
func (l *level) front() *resting { return l.head }

func (l *level) empty() bool { return l.head == nil }

// levelHeap is a binary heap of levels ordered by price: bids descending,
// asks ascending. It holds level pointers rather than prices, so reaching
// the best level is a slice read with no map lookup. It is hand-rolled
// rather than built on container/heap, which moves values through
// interface{}.
type levelHeap struct {
	lvls []*level
	desc bool
}

func (h *levelHeap) Len() int { return len(h.lvls) }

func (h *levelHeap) less(i, j int) bool {
	if h.desc {
		return h.lvls[i].price > h.lvls[j].price
	}
	return h.lvls[i].price < h.lvls[j].price
}

func (h *levelHeap) push(lv *level) {
	h.lvls = append(h.lvls, lv)
	i := len(h.lvls) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if !h.less(i, parent) {
			break
		}
		h.lvls[i], h.lvls[parent] = h.lvls[parent], h.lvls[i]
		i = parent
	}
}

// pop removes the root.
func (h *levelHeap) pop() {
	n := len(h.lvls) - 1
	h.lvls[0] = h.lvls[n]
	h.lvls[n] = nil
	h.lvls = h.lvls[:n]
	if n > 1 {
		h.siftDown(0)
	}
}

func (h *levelHeap) siftDown(i int) {
	n := len(h.lvls)
	for {
		l, r := 2*i+1, 2*i+2
		best := i
		if l < n && h.less(l, best) {
			best = l
		}
		if r < n && h.less(r, best) {
			best = r
		}
		if best == i {
			return
		}
		h.lvls[i], h.lvls[best] = h.lvls[best], h.lvls[i]
		i = best
	}
}

// sideBook is one side of the order book. The map finds a level by price
// when an order rests; the heap gives the best level directly. Each level
// is in both exactly once while it exists. Emptied levels are pruned
// lazily when they reach the top of the heap.
type sideBook struct {
	levels map[int64]*level
	heap   levelHeap
	pool   *pool
}

func newSideBook(desc bool, p *pool) *sideBook {
	return &sideBook{levels: make(map[int64]*level), heap: levelHeap{desc: desc}, pool: p}
}

func (sb *sideBook) add(r *resting) {
	lv, ok := sb.levels[r.price]
	if !ok {
		lv = sb.pool.getLevel(r.price)
		sb.levels[r.price] = lv
		sb.heap.push(lv)
	}
	r.lv = lv
	lv.push(r)
}

// best returns the best-priced non-empty level, pruning dead levels.
func (sb *sideBook) best() *level {
	for sb.heap.Len() > 0 {
		lv := sb.heap.lvls[0]
		if !lv.empty() {
			return lv
		}
		sb.heap.pop()
		delete(sb.levels, lv.price)
		sb.pool.putLevel(lv)
	}
	return nil
}

// Depth is an aggregated view of one price level, for snapshots.
type Depth struct {
	Price int64
	Qty   int64
}
