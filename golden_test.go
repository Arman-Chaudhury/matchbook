package matchbook

import (
	"encoding/binary"
	"hash/fnv"
	"math/rand"
	"testing"
)

// goldenHash is the FNV-64a digest of every result the engine produced for
// the fixed stream below when this test was written. The engine's whole
// value is determinism: the same input must produce exactly the same
// trades forever. If this test fails, behaviour changed. Update the
// constant only when that change is intended and documented.
const goldenHash uint64 = 0xbe08b892011c379f

func TestGoldenDeterminism(t *testing.T) {
	h := fnv.New64a()
	buf := make([]byte, 8)
	w := func(v int64) {
		binary.LittleEndian.PutUint64(buf, uint64(v))
		h.Write(buf)
	}
	rng := rand.New(rand.NewSource(2026))
	eng := NewEngine()
	var nextID int64
	var live []int64
	for op := 0; op < 50_000; op++ {
		if rng.Intn(10) == 0 && len(live) > 0 {
			id := live[rng.Intn(len(live))]
			w(-id)
			if eng.Cancel(id) {
				w(1)
			} else {
				w(0)
			}
			continue
		}
		nextID++
		o := Order{ID: nextID, Side: Side(rng.Intn(2)), Type: Limit, Qty: int64(1 + rng.Intn(100))}
		if rng.Intn(8) == 0 {
			o.Type = Market
		} else {
			o.Price = int64(1000 + rng.Intn(40))
			live = append(live, o.ID)
		}
		res := eng.Process(o)
		w(int64(res.Status))
		w(res.Remaining)
		for _, tr := range res.Trades {
			w(tr.TakerID)
			w(tr.MakerID)
			w(tr.Price)
			w(tr.Qty)
		}
	}
	for _, side := range []Side{Buy, Sell} {
		for _, d := range eng.Depth(side, 100) {
			w(d.Price)
			w(d.Qty)
		}
	}
	if got := h.Sum64(); got != goldenHash {
		t.Fatalf("golden hash = %#x, want %#x: engine behaviour changed", got, goldenHash)
	}
}
