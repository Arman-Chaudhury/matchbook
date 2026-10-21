package matchbook

import "testing"

// FuzzEngineVsReference decodes arbitrary bytes into an operation stream and
// requires the engine and the naive reference model to agree on every
// result, so the fuzzer can hunt for sequences the random property suite
// never generates. The seed corpus runs as a normal test; to search, run:
//
//	go test -fuzz=FuzzEngineVsReference -fuzztime=30s
func FuzzEngineVsReference(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x01, 0x01, 0x10, 0x05, 0x02, 0x00, 0x12, 0x05})
	f.Add([]byte{0x01, 0x01, 0x10, 0x05, 0x01, 0x01, 0x12, 0x05, 0x01, 0x80, 0, 0})
	f.Add([]byte{0x03, 0x02, 0x00, 0x09, 0x03, 0x00, 0x00, 0x00, 0x04, 0x01, 0x01, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		eng := NewEngine()
		ref := &refBook{}
		for len(data) >= 4 {
			id, kind, px, qty := int64(data[0]), data[1], int64(data[2]), int64(data[3])
			data = data[4:]
			if kind&0x80 != 0 { // cancel
				if got, want := eng.Cancel(id), ref.cancel(id); got != want {
					t.Fatalf("cancel(%d) = %v, ref %v", id, got, want)
				}
				continue
			}
			// Prices and quantities are shifted down by one so zero and
			// negative values, which must be rejected, are reachable.
			o := Order{ID: id, Side: Side(kind & 1), Type: Type((kind >> 1) & 1), Qty: qty - 1}
			if o.Type == Limit {
				o.Price = px - 1
			}
			got := eng.Process(o)
			got.Trades = append([]Trade(nil), got.Trades...)
			if want := ref.process(o); !sameResult(got, want) {
				t.Fatalf("order %+v\nengine: %+v\nref:    %+v", o, got, want)
			}
			if bid, _, ok1 := eng.BestBid(); ok1 {
				if ask, _, ok2 := eng.BestAsk(); ok2 && bid >= ask {
					t.Fatalf("crossed book bid=%d ask=%d", bid, ask)
				}
			}
		}
	})
}
