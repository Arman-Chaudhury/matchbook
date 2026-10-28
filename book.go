package matchbook

// resting is an order sitting on the book. Cancelled orders are tombstoned
// and skipped lazily when their level reaches the front of the queue.
type resting struct {
	id        int64
	owner     int64
	side      Side
	price     int64
	qty       int64
	seq       uint64 // arrival sequence: FIFO within a price level
	cancelled bool
	lv        *level // the level this order sits in, so cancel needs no lookup
}

// pool recycles resting orders and price levels so that steady-state
// trading does not allocate. A resting order goes back to the pool at the
// one moment nothing can reach it any more: when its level's head index
// moves past it. Before that it may be a tombstone still sitting in the
// queue; after it, only slots behind head point at it, and those are never
// read again (compaction copies from head onward). A level goes back when
// its side book prunes it as empty.
type pool struct {
	orders []*resting
	levels []*level
}

// maxPooledQueue caps the queue capacity a recycled level keeps, so one
// burst at a single price does not pin a large array forever.
const maxPooledQueue = 1024

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
	clear(lv.orders)
	if cap(lv.orders) > maxPooledQueue {
		lv.orders = nil
	} else {
		lv.orders = lv.orders[:0]
	}
	lv.head, lv.qty, lv.dead = 0, 0, 0
	p.levels = append(p.levels, lv)
}

// level is a FIFO queue of resting orders at one price. head advances
// instead of re-slicing so dequeue is O(1) without reallocating.
type level struct {
	price  int64
	orders []*resting
	head   int
	qty    int64 // live quantity at this level (excludes tombstones)
	dead   int   // tombstones at or after head
	pool   *pool
}

func (l *level) push(r *resting) {
	l.orders = append(l.orders, r)
	l.qty += r.qty
}

// pop advances head past the order at the front, which the caller has
// finished with, and recycles it.
func (l *level) pop() {
	r := l.orders[l.head]
	if r.cancelled {
		l.dead--
	}
	l.pool.putOrder(r)
	l.head++
}

// tombstone records that an order somewhere in this queue was cancelled.
// Head-based compaction only reclaims dead slots once head reaches them,
// which never happens while the order at the front stays live. So when
// tombstones pass compactAt and outnumber live orders, sweep them out.
func (l *level) tombstone() {
	l.dead++
	if l.dead >= compactAt && l.dead*2 > len(l.orders)-l.head {
		l.sweep()
	}
}

// sweep rewrites the queue keeping only live orders, in their original
// order, and recycles the tombstones. Cost is linear in the queue, paid at
// most once per compactAt cancels, so amortised O(1) per cancel.
func (l *level) sweep() {
	n := 0
	for _, r := range l.orders[l.head:] {
		if r.cancelled {
			l.pool.putOrder(r)
			continue
		}
		l.orders[n] = r
		n++
	}
	clear(l.orders[n:])
	l.orders = l.orders[:n]
	l.head, l.dead = 0, 0
}

// compactAt is the number of consumed slots a level tolerates before its
// queue is compacted. Without compaction a busy price level keeps every
// dead order it ever held until the level itself empties, which at an
// active price may never happen.
const compactAt = 64

// front returns the first live order at this level, recycling tombstones
// it skips.
func (l *level) front() *resting {
	for l.head < len(l.orders) {
		r := l.orders[l.head]
		if !r.cancelled && r.qty > 0 {
			break
		}
		l.pop()
	}
	if l.head >= compactAt && l.head*2 >= len(l.orders) {
		l.compact()
	}
	if l.head < len(l.orders) {
		return l.orders[l.head]
	}
	return nil
}

// compact drops the consumed prefix of the queue. Those slots point at
// orders already recycled, so they are cleared, not freed.
func (l *level) compact() {
	n := copy(l.orders, l.orders[l.head:])
	clear(l.orders[n:])
	l.orders = l.orders[:n]
	l.head = 0
}

func (l *level) empty() bool { return l.front() == nil }

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
