package matchbook

import "container/heap"

// resting is an order sitting on the book. Cancelled orders are tombstoned
// and skipped lazily when their level reaches the front of the queue.
type resting struct {
	id        int64
	price     int64
	qty       int64
	seq       uint64 // arrival sequence: FIFO within a price level
	cancelled bool
}

// level is a FIFO queue of resting orders at one price. head advances
// instead of re-slicing so dequeue is O(1) without reallocating.
type level struct {
	price  int64
	orders []*resting
	head   int
	qty    int64 // live quantity at this level (excludes tombstones)
}

func (l *level) push(r *resting) {
	l.orders = append(l.orders, r)
	l.qty += r.qty
}

// front returns the first live order at this level, skipping tombstones.
func (l *level) front() *resting {
	for l.head < len(l.orders) {
		r := l.orders[l.head]
		if !r.cancelled && r.qty > 0 {
			return r
		}
		l.head++
	}
	return nil
}

func (l *level) empty() bool { return l.front() == nil }

// priceHeap orders bid prices descending and ask prices ascending.
type priceHeap struct {
	prices []int64
	desc   bool
}

func (h priceHeap) Len() int { return len(h.prices) }
func (h priceHeap) Less(i, j int) bool {
	if h.desc {
		return h.prices[i] > h.prices[j]
	}
	return h.prices[i] < h.prices[j]
}
func (h priceHeap) Swap(i, j int)       { h.prices[i], h.prices[j] = h.prices[j], h.prices[i] }
func (h *priceHeap) Push(x interface{}) { h.prices = append(h.prices, x.(int64)) }
func (h *priceHeap) Pop() interface{} {
	old := h.prices
	n := len(old)
	x := old[n-1]
	h.prices = old[:n-1]
	return x
}

// sideBook is one side of the order book: price levels keyed by price,
// with a heap giving best-price access. Heap entries for emptied levels
// are discarded lazily on access.
type sideBook struct {
	levels map[int64]*level
	heap   priceHeap
}

func newSideBook(desc bool) *sideBook {
	return &sideBook{levels: make(map[int64]*level), heap: priceHeap{desc: desc}}
}

func (sb *sideBook) add(r *resting) {
	lv, ok := sb.levels[r.price]
	if !ok {
		lv = &level{price: r.price}
		sb.levels[r.price] = lv
		heap.Push(&sb.heap, r.price)
	}
	lv.push(r)
}

// best returns the best-priced non-empty level, pruning dead levels.
func (sb *sideBook) best() *level {
	for sb.heap.Len() > 0 {
		price := sb.heap.prices[0]
		lv, ok := sb.levels[price]
		if ok && !lv.empty() {
			return lv
		}
		heap.Pop(&sb.heap)
		delete(sb.levels, price)
	}
	return nil
}

// Depth is an aggregated view of one price level, for snapshots.
type Depth struct {
	Price int64
	Qty   int64
}
