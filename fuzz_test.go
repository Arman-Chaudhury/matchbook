package matchbook

import "testing"

// FuzzEngineVsReference decodes arbitrary bytes into an operation stream and
// requires the engine and the naive reference model to agree on every
// result, so the fuzzer can hunt for sequences the random property suite
// never generates. The seed corpus runs as a normal test; to search, run:
//
//	go test -fuzz=FuzzEngineVsReference -fuzztime=30s
//
// Encoding: byte 0 picks the STP policy; then 6-byte ops:
//
//	id  kind  price  qty  flags  owner
//
// kind: bit7 cancel, bit6 replace, bit0 side, bit1 type.
// flags: bits 0-1 TIF, bit 2 post-only. Prices and quantities are shifted
// down by one so zero and negative values, which must be rejected, are
// reachable.
func FuzzEngineVsReference(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 0x01, 0x10, 0x05, 0, 1, 2, 0x00, 0x12, 0x05, 0, 1})
	f.Add([]byte{1, 1, 0x01, 0x10, 0x05, 0, 1, 2, 0x00, 0x12, 0x05, 0, 1, 1, 0x80, 0, 0, 0, 0})
	f.Add([]byte{2, 3, 0x02, 0x00, 0x09, 1, 1, 3, 0x00, 0x00, 0x00, 2, 1, 4, 0x01, 0x01, 0x01, 4, 1})
	f.Add([]byte{0, 1, 0x01, 0x10, 0x05, 0, 0, 1, 0x40, 0x10, 0x03, 0, 0, 1, 0x40, 0x11, 0x09, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		stp := STPMode(data[0] % 3)
		data = data[1:]
		eng := NewEngine(WithSelfTradePrevention(stp))
		ref := &refBook{stp: stp}
		for len(data) >= 6 {
			id, kind, px, qty := int64(data[0]), data[1], int64(data[2])-1, int64(data[3])-1
			flags, owner := data[4], int64(data[5])
			data = data[6:]
			switch {
			case kind&0x80 != 0: // cancel
				if got, want := eng.Cancel(id), ref.cancel(id); got != want {
					t.Fatalf("cancel(%d) = %v, ref %v", id, got, want)
				}
				continue
			case kind&0x40 != 0: // replace
				got := eng.Replace(id, px, qty)
				got.Trades = append([]Trade(nil), got.Trades...)
				if want := ref.replace(id, px, qty); !sameResult(got, want) {
					t.Fatalf("replace(%d,%d,%d)\nengine: %+v\nref:    %+v", id, px, qty, got, want)
				}
			default:
				o := Order{
					ID: id, Owner: owner, Side: Side(kind & 1), Type: Type((kind >> 1) & 1),
					TIF: TimeInForce(flags & 3), PostOnly: flags&4 != 0, Qty: qty,
				}
				if o.Type == Limit {
					o.Price = px
				}
				got := eng.Process(o)
				got.Trades = append([]Trade(nil), got.Trades...)
				if want := ref.process(o); !sameResult(got, want) {
					t.Fatalf("order %+v\nengine: %+v\nref:    %+v", o, got, want)
				}
			}
			if bid, _, ok1 := eng.BestBid(); ok1 {
				if ask, _, ok2 := eng.BestAsk(); ok2 && bid >= ask {
					t.Fatalf("crossed book bid=%d ask=%d", bid, ask)
				}
			}
		}
	})
}
