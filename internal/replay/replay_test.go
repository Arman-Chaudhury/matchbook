package replay

import (
	"bytes"
	"testing"

	"github.com/Arman-Chaudhury/matchbook/internal/itch"
)

const loc = 5

// feed builds a framed ITCH stream for one symbol, with timestamps that
// increase by one per message.
type feed struct {
	buf bytes.Buffer
	ts  uint64
}

func newFeed() *feed {
	f := &feed{}
	f.put(itch.EncodeSystemEvent(f.tick(), 'Q'))
	f.put(itch.EncodeStockDirectory(loc, f.tick(), "TEST"))
	return f
}

func (f *feed) tick() uint64   { f.ts++; return f.ts }
func (f *feed) put(m itch.Msg) { itch.WriteMsg(&f.buf, m) }
func (f *feed) add(ref uint64, buy bool, shares, price uint32) {
	f.put(itch.EncodeAddOrder(loc, f.tick(), ref, buy, shares, "TEST", price))
}
func (f *feed) exec(ref uint64, shares uint32) {
	f.put(itch.EncodeExecuted(loc, f.tick(), ref, shares, f.ts))
}

func run(t *testing.T, f *feed) *Symbol {
	t.Helper()
	res, err := Feed(bytes.NewReader(f.buf.Bytes()), nil, Options{DepthEvery: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Symbols) != 1 {
		t.Fatalf("%d symbols", len(res.Symbols))
	}
	return res.Symbols[0]
}

// clean asserts the book never disagreed with the feed.
func clean(t *testing.T, s *Symbol) {
	t.Helper()
	st := s.Stats
	if st.TopMismatches+st.DepthMismatch+st.UnknownRef+st.ExecWrongFill != 0 {
		t.Fatalf("book disagreed with feed: %+v\n%+v", st, s.Mismatches())
	}
}

func TestExecutionsInPriorityOrderAllMatch(t *testing.T) {
	f := newFeed()
	f.add(1, false, 100, 1_000_000) // ask 100.00
	f.add(2, false, 50, 1_000_000)  // same price, behind 1
	f.add(3, false, 70, 1_000_100)  // worse price
	f.add(4, true, 30, 999_900)     // bid
	f.exec(1, 40)                   // partial
	f.exec(1, 60)                   // rest of 1
	f.exec(2, 50)                   // then 2
	f.exec(3, 10)                   // then the next level
	f.exec(4, 30)                   // a bid
	s := run(t, f)
	clean(t, s)
	if s.Stats.ExecMatched != 5 || s.Stats.Executions != 5 {
		t.Fatalf("%+v %v", s.Stats, s.Mismatches())
	}
	if s.RestingOrders() != 1 { // order 3 has 60 left
		t.Fatalf("resting %d", s.RestingOrders())
	}
	if _, q, _ := s.Engine.BestAsk(); q != 60 {
		t.Fatalf("ask qty %d", q)
	}
}

func TestPriorityViolationIsDetectedAndBookStaysInSync(t *testing.T) {
	f := newFeed()
	f.add(1, true, 100, 500_000)
	f.add(2, true, 100, 500_000) // behind 1, and a higher reference
	f.add(3, true, 100, 499_900) // worse price
	f.exec(2, 100)               // feed says 2 traded before 1
	f.exec(3, 10)                // and 3 traded while 1 was better
	s := run(t, f)
	clean(t, s)
	if s.Stats.ExecNotFront != 1 || s.Stats.ExecReceiptOrder != 0 || s.Stats.ExecNotBestPrice != 1 || s.Stats.ExecMatched != 0 {
		t.Fatalf("%+v", s.Stats)
	}
	if s.Stats.Unexplained() != 2 {
		t.Fatalf("unexplained %d, want 2", s.Stats.Unexplained())
	}
	if len(s.Mismatches()) != 2 {
		t.Fatalf("mismatches %v", s.Mismatches())
	}
}

// TestReceiptOrderPriorityIsExplained models what the real feed shows: an
// order NASDAQ received earlier (lower reference) is published after a
// later one at the same price, and NASDAQ executes it first. The engine
// disagrees, the replay classifies it as receipt-order priority, and the
// book stays in sync.
func TestReceiptOrderPriorityIsExplained(t *testing.T) {
	f := newFeed()
	f.add(50, false, 100, 700_000) // received later, published first
	f.add(10, false, 100, 700_000) // received earlier, published second
	f.exec(10, 100)                // NASDAQ honours receipt order
	f.exec(50, 100)
	s := run(t, f)
	clean(t, s)
	if s.Stats.ExecNotFront != 1 || s.Stats.ExecReceiptOrder != 1 || s.Stats.ExecMatched != 1 {
		t.Fatalf("%+v %v", s.Stats, s.Mismatches())
	}
	if s.Stats.Unexplained() != 0 || s.Mismatches()[0].Kind != "exec-receipt-order" {
		t.Fatalf("unexplained %d %v", s.Stats.Unexplained(), s.Mismatches())
	}
}

func TestPartialCancelKeepsPriorityAndReplaceLosesIt(t *testing.T) {
	f := newFeed()
	f.add(1, false, 100, 200_000)
	f.add(2, false, 100, 200_000)
	f.put(itch.EncodeOrderCancel(loc, f.tick(), 1, 60)) // 1 shrinks, keeps its place
	f.exec(1, 40)                                       // so 1 still trades first
	f.add(3, false, 100, 200_000)
	f.put(itch.EncodeOrderReplace(loc, f.tick(), 2, 9, 100, 200_000)) // 2 -> 9, to the back
	f.exec(3, 100)                                                    // so 3 is now first
	f.exec(9, 100)
	s := run(t, f)
	clean(t, s)
	if s.Stats.ExecMatched != 3 || s.Stats.ExecNotFront != 0 {
		t.Fatalf("%+v %v", s.Stats, s.Mismatches())
	}
	if s.RestingOrders() != 0 {
		t.Fatalf("resting %d", s.RestingOrders())
	}
}

func TestCrossExecutionsDeletesAndHiddenTrades(t *testing.T) {
	f := newFeed()
	f.add(1, true, 100, 300_000)
	f.add(2, false, 100, 300_100)
	f.put(itch.EncodeExecutedWithPrice(loc, f.tick(), 1, 100, 1, 300_050)) // auction print
	f.put(itch.EncodeOrderDelete(loc, f.tick(), 2))
	f.put(itch.EncodeTradingAction(loc, f.tick(), "TEST", 'H'))
	s := run(t, f)
	clean(t, s)
	if s.Stats.ExecWithPrice != 1 || s.Stats.Deletes != 1 || s.Stats.TradingActions != 1 {
		t.Fatalf("%+v", s.Stats)
	}
	if _, _, ok := s.Engine.BestBid(); ok {
		t.Fatal("book should be empty")
	}
}

func TestCrossingAddIsRefusedAndCounted(t *testing.T) {
	f := newFeed()
	f.add(1, false, 100, 100_000)
	f.add(2, true, 50, 100_100) // would trade with 1
	f.exec(2, 50)               // the feed later executes it
	f.exec(1, 100)
	s := run(t, f)
	clean(t, s)
	if s.Stats.AddsWouldCross != 1 || s.Stats.ExecOnUnplaced != 1 || s.Stats.ExecMatched != 1 {
		t.Fatalf("%+v %v", s.Stats, s.Mismatches())
	}
}

// TestCheckersCatchATamperedBook proves the book checks are not vacuous:
// an order removed from the engine behind the replay's back must show up
// as top-of-book and depth disagreements, and a later execution of it as
// an engine-lost-order mismatch.
func TestCheckersCatchATamperedBook(t *testing.T) {
	s := NewSymbol("TEST", Options{DepthEvery: 1, KeepPerKind: 1})
	s.Apply(itch.EncodeAddOrder(loc, 1, 1, true, 100, "TEST", 500_000))
	s.Apply(itch.EncodeAddOrder(loc, 2, 2, true, 100, "TEST", 499_000))
	if s.Stats.TopMismatches+s.Stats.DepthMismatch != 0 {
		t.Fatalf("clean so far: %+v", s.Stats)
	}
	s.Engine.Cancel(1) // tamper
	s.Apply(itch.EncodeAddOrder(loc, 3, 3, false, 10, "TEST", 600_000))
	if s.Stats.TopMismatches == 0 || s.Stats.DepthMismatch == 0 {
		t.Fatalf("tampering went unnoticed: %+v", s.Stats)
	}
	s.Apply(itch.EncodeAddOrder(loc, 4, 4, false, 10, "TEST", 600_100)) // still diverged
	s.Apply(itch.EncodeOrderDelete(loc, 5, 1))                          // feed catches up
	kinds := map[string]bool{}
	for _, m := range s.Mismatches() {
		kinds[m.Kind] = true
	}
	for _, k := range []string{"top-of-book", "depth", "engine-lost-order"} {
		if !kinds[k] {
			t.Fatalf("no %q mismatch recorded: %v", k, s.Mismatches())
		}
	}
	// KeepPerKind caps the record, not the count.
	n := 0
	for _, m := range s.Mismatches() {
		if m.Kind == "top-of-book" {
			n++
		}
	}
	if n != 1 || s.Stats.TopMismatches < 2 {
		t.Fatalf("kept %d top-of-book mismatches, counted %d", n, s.Stats.TopMismatches)
	}
}

func TestOverReduceIsClampedAndRecorded(t *testing.T) {
	f := newFeed()
	f.add(1, true, 10, 100_000)
	f.put(itch.EncodeOrderCancel(loc, f.tick(), 1, 25))
	s := run(t, f)
	clean(t, s)
	if len(s.Mismatches()) != 1 || s.Mismatches()[0].Kind != "over-reduce" || s.RestingOrders() != 0 {
		t.Fatalf("%v", s.Mismatches())
	}
}

func TestUnknownReferencesAreCounted(t *testing.T) {
	f := newFeed()
	f.exec(77, 1)
	f.put(itch.EncodeOrderDelete(loc, f.tick(), 78))
	s := run(t, f)
	if s.Stats.UnknownRef != 2 {
		t.Fatalf("%+v", s.Stats)
	}
}

func TestOnlyFilterAndSymbolRouting(t *testing.T) {
	var buf bytes.Buffer
	itch.WriteMsg(&buf, itch.EncodeStockDirectory(1, 1, "AAA"))
	itch.WriteMsg(&buf, itch.EncodeStockDirectory(2, 2, "BBB"))
	itch.WriteMsg(&buf, itch.EncodeAddOrder(1, 3, 10, true, 5, "AAA", 100))
	itch.WriteMsg(&buf, itch.EncodeAddOrder(2, 4, 11, true, 7, "BBB", 100))
	res, err := Feed(bytes.NewReader(buf.Bytes()), map[string]bool{"BBB": true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Symbols) != 1 || res.Symbols[0].Name != "BBB" || res.Symbols[0].Stats.Adds != 1 {
		t.Fatalf("%+v", res.Symbols)
	}
	if res.Messages != 4 {
		t.Fatalf("messages %d", res.Messages)
	}
}

func TestReplayIsDeterministic(t *testing.T) {
	f := newFeed()
	for i := uint64(1); i <= 200; i++ {
		buy := i%2 == 0
		price := uint32(1_000_100 + (i%5)*100) // asks above 100.00
		if buy {
			price = uint32(1_000_000 - (i%5)*100) // bids at or below
		}
		f.add(i, buy, uint32(10+i%7), price)
		if i%3 == 0 {
			f.exec(i-2, 1)
		}
	}
	a, b := run(t, f), run(t, f)
	if a.Stats.Adds != 200 || a.Stats.Executions == 0 {
		t.Fatalf("%+v", a.Stats)
	}
	if a.Engine.Digest() != b.Engine.Digest() {
		t.Fatal("same feed, different digests")
	}
}
