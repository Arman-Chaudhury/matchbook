package matchbook

import (
	"bytes"
	"errors"
	"math/rand"
	"reflect"
	"testing"
)

// stripSeq returns a copy of evs with Seq zeroed so tests can compare the
// facts without hard-coding sequence numbers.
func stripSeq(evs []Event) []Event {
	out := make([]Event, len(evs))
	for i, ev := range evs {
		ev.Seq = 0
		out[i] = ev
	}
	return out
}

func TestEventsPerOutcome(t *testing.T) {
	e := NewEngine()

	e.Process(limit(1, Sell, 100, 5))
	want := []Event{
		{Kind: EvAccepted, OrderID: 1, Side: Sell, Type: Limit, Price: 100, Qty: 5},
		{Kind: EvRested, OrderID: 1, Side: Sell, Price: 100, Qty: 5},
	}
	if got := stripSeq(e.Events()); !reflect.DeepEqual(got, want) {
		t.Fatalf("rest:\n got %+v\nwant %+v", got, want)
	}

	e.Process(limit(2, Buy, 101, 8)) // fills 5 at 100, rests 3 at 101
	want = []Event{
		{Kind: EvAccepted, OrderID: 2, Side: Buy, Type: Limit, Price: 101, Qty: 8},
		{Kind: EvFill, OrderID: 2, MakerID: 1, Price: 100, Qty: 5},
		{Kind: EvRested, OrderID: 2, Side: Buy, Price: 101, Qty: 3},
	}
	if got := stripSeq(e.Events()); !reflect.DeepEqual(got, want) {
		t.Fatalf("partial fill:\n got %+v\nwant %+v", got, want)
	}

	e.Process(market(3, Sell, 10)) // fills 3, discards 7
	want = []Event{
		{Kind: EvAccepted, OrderID: 3, Side: Sell, Type: Market, Qty: 10},
		{Kind: EvFill, OrderID: 3, MakerID: 2, Price: 101, Qty: 3},
		{Kind: EvDiscarded, OrderID: 3, Qty: 7},
	}
	if got := stripSeq(e.Events()); !reflect.DeepEqual(got, want) {
		t.Fatalf("market:\n got %+v\nwant %+v", got, want)
	}

	e.Process(limit(4, Buy, 0, 1))
	want = []Event{{Kind: EvRejected, OrderID: 4, Reject: RejectPrice}}
	if got := stripSeq(e.Events()); !reflect.DeepEqual(got, want) {
		t.Fatalf("reject:\n got %+v\nwant %+v", got, want)
	}

	e.Process(limit(5, Buy, 99, 4))
	e.Cancel(5)
	want = []Event{{Kind: EvCancelled, OrderID: 5, Side: Buy, Price: 99, Qty: 4}}
	if got := stripSeq(e.Events()); !reflect.DeepEqual(got, want) {
		t.Fatalf("cancel:\n got %+v\nwant %+v", got, want)
	}

	e.Cancel(5)
	want = []Event{{Kind: EvCancelRejected, OrderID: 5}}
	if got := stripSeq(e.Events()); !reflect.DeepEqual(got, want) {
		t.Fatalf("cancel reject:\n got %+v\nwant %+v", got, want)
	}

	// A fully filled order emits Accepted and Fills, nothing else.
	e.Process(limit(6, Sell, 100, 1))
	e.Process(limit(7, Buy, 100, 1))
	want = []Event{
		{Kind: EvAccepted, OrderID: 7, Side: Buy, Type: Limit, Price: 100, Qty: 1},
		{Kind: EvFill, OrderID: 7, MakerID: 6, Price: 100, Qty: 1},
	}
	if got := stripSeq(e.Events()); !reflect.DeepEqual(got, want) {
		t.Fatalf("full fill:\n got %+v\nwant %+v", got, want)
	}
}

func TestEventSeqIsContiguous(t *testing.T) {
	e := NewEngine()
	var next uint64 = 1
	for i := int64(1); i <= 200; i++ {
		e.Process(Order{ID: i, Side: Side(i % 2), Type: Limit, Price: 100 + i%3, Qty: 3})
		for _, ev := range e.Events() {
			if ev.Seq != next {
				t.Fatalf("event seq %d, want %d", ev.Seq, next)
			}
			next++
		}
		if i%7 == 0 {
			e.Cancel(i - 3)
			for _, ev := range e.Events() {
				if ev.Seq != next {
					t.Fatalf("event seq %d, want %d", ev.Seq, next)
				}
				next++
			}
		}
	}
	if e.EventSeq() != next-1 {
		t.Fatalf("EventSeq = %d, want %d", e.EventSeq(), next-1)
	}
}

func TestDigestDependsOnOrderAndContent(t *testing.T) {
	a, b, c := NewEngine(), NewEngine(), NewEngine()
	a.Process(limit(1, Buy, 100, 1))
	a.Process(limit(2, Sell, 101, 1))
	b.Process(limit(2, Sell, 101, 1))
	b.Process(limit(1, Buy, 100, 1))
	c.Process(limit(1, Buy, 100, 1))
	c.Process(limit(2, Sell, 101, 2))
	if a.Digest() == b.Digest() || a.Digest() == c.Digest() || NewEngine().Digest() == a.Digest() {
		t.Fatalf("digests should differ: %x %x %x", a.Digest(), b.Digest(), c.Digest())
	}
}

func TestApplyMatchesDirectCalls(t *testing.T) {
	direct, applied := NewEngine(), NewEngine()
	cmds := []Command{
		{Kind: CmdSubmit, Order: limit(1, Sell, 100, 5)},
		{Kind: CmdSubmit, Order: limit(2, Buy, 100, 2)},
		{Kind: CmdCancel, Order: Order{ID: 1}},
		{Kind: CmdCancel, Order: Order{ID: 1}},
		{Kind: CmdSubmit, Order: market(3, Buy, 1)},
	}
	for _, c := range cmds {
		if c.Kind == CmdCancel {
			direct.Cancel(c.Order.ID)
		} else {
			direct.Process(c.Order)
		}
		got := applied.Apply(c)
		if !reflect.DeepEqual(got, direct.Events()) {
			t.Fatalf("Apply(%+v) events %+v, direct %+v", c, got, direct.Events())
		}
	}
	if direct.Digest() != applied.Digest() {
		t.Fatal("digests differ")
	}
}

// randomCommands generates a seeded command stream with the same shape as
// the property suite: mostly limits in a tight band, some markets, some
// cancels, a few duplicate IDs and bad prices.
func randomCommands(seed int64, n int) []Command {
	rng := rand.New(rand.NewSource(seed))
	cmds := make([]Command, 0, n)
	var nextID int64
	var live []int64
	for i := 0; i < n; i++ {
		if rng.Intn(10) == 0 && len(live) > 0 {
			cmds = append(cmds, Command{Kind: CmdCancel, Order: Order{ID: live[rng.Intn(len(live))]}})
			continue
		}
		o := Order{Side: Side(rng.Intn(2)), Type: Limit, Qty: int64(1 + rng.Intn(50))}
		if rng.Intn(25) == 0 && len(live) > 0 {
			o.ID = live[rng.Intn(len(live))]
		} else {
			nextID++
			o.ID = nextID
		}
		switch {
		case rng.Intn(8) == 0:
			o.Type = Market
		case rng.Intn(40) == 0:
			o.Price = 0
		default:
			o.Price = int64(priceFloor + rng.Intn(priceBand))
			live = append(live, o.ID)
		}
		cmds = append(cmds, Command{Kind: CmdSubmit, Order: o})
	}
	return cmds
}

func TestJournalReplayReproducesDigest(t *testing.T) {
	for seed := int64(0); seed < 20; seed++ {
		cmds := randomCommands(seed, 2000)
		var buf bytes.Buffer
		j, err := NewJournal(&buf)
		if err != nil {
			t.Fatal(err)
		}
		live := NewEngine()
		var allEvents []Event
		for _, c := range cmds {
			if err := j.Append(c); err != nil {
				t.Fatal(err)
			}
			allEvents = append(allEvents, live.Apply(c)...)
		}
		if got := buf.Len(); got != len(journalMagic)+recordSize*len(cmds) {
			t.Fatalf("journal size %d, want %d", got, len(journalMagic)+recordSize*len(cmds))
		}

		replayed := NewEngine()
		n, err := Replay(bytes.NewReader(buf.Bytes()), replayed)
		if err != nil || n != len(cmds) {
			t.Fatalf("seed %d: replay n=%d err=%v", seed, n, err)
		}
		if replayed.Digest() != live.Digest() || replayed.EventSeq() != live.EventSeq() {
			t.Fatalf("seed %d: replay digest %x/%d, live %x/%d", seed,
				replayed.Digest(), replayed.EventSeq(), live.Digest(), live.EventSeq())
		}
		if uint64(len(allEvents)) != live.EventSeq() {
			t.Fatalf("seed %d: %d events collected, EventSeq %d", seed, len(allEvents), live.EventSeq())
		}
		for _, side := range []Side{Buy, Sell} {
			if a, b := live.Depth(side, 50), replayed.Depth(side, 50); !reflect.DeepEqual(a, b) {
				t.Fatalf("seed %d: %v depth differs\nlive %v\nreplay %v", seed, side, a, b)
			}
		}
	}
}

func TestReplayErrors(t *testing.T) {
	if _, err := Replay(bytes.NewReader(nil), NewEngine()); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := Replay(bytes.NewReader([]byte("nope")), NewEngine()); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("wrong magic: %v", err)
	}

	var buf bytes.Buffer
	j, _ := NewJournal(&buf)
	j.Append(Command{Kind: CmdSubmit, Order: limit(1, Buy, 100, 1)})
	j.Append(Command{Kind: CmdSubmit, Order: limit(2, Buy, 100, 1)})
	torn := buf.Bytes()[:buf.Len()-5] // crash mid-record
	e := NewEngine()
	n, err := Replay(bytes.NewReader(torn), e)
	if !errors.Is(err, ErrTornRecord) || n != 1 {
		t.Fatalf("torn: n=%d err=%v", n, err)
	}
	if _, q, ok := e.BestBid(); !ok || q != 1 {
		t.Fatalf("the whole record before the tear should have applied, bid qty %d", q)
	}

	corrupt := append([]byte(nil), buf.Bytes()...)
	corrupt[len(journalMagic)+1] = 9 // side out of range
	if n, err := Replay(bytes.NewReader(corrupt), NewEngine()); err == nil || n != 0 {
		t.Fatalf("corrupt record: n=%d err=%v", n, err)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	e := NewEngine()
	e.Process(limit(1, Sell, 102, 5))
	e.Process(limit(2, Sell, 102, 7)) // same level, behind 1
	e.Process(limit(3, Buy, 100, 4))
	e.Process(limit(4, Buy, 99, 2))
	e.Process(limit(5, Buy, 100, 1))
	e.Cancel(4)
	e.Process(market(6, Sell, 2)) // takes 2 from order 3

	var buf bytes.Buffer
	if err := e.Snapshot(&buf); err != nil {
		t.Fatal(err)
	}
	r, err := Restore(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if r.Digest() != e.Digest() || r.EventSeq() != e.EventSeq() {
		t.Fatal("counters not restored")
	}
	for _, side := range []Side{Buy, Sell} {
		if a, b := e.Depth(side, 10), r.Depth(side, 10); !reflect.DeepEqual(a, b) {
			t.Fatalf("%v depth: %v vs %v", side, a, b)
		}
	}
	// FIFO within the ask level must survive: order 1 fills before order 2.
	got := r.Process(market(7, Buy, 6)).Trades
	want := []Trade{{TakerID: 7, MakerID: 1, Price: 102, Qty: 5}, {TakerID: 7, MakerID: 2, Price: 102, Qty: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trades after restore %v, want %v", got, want)
	}
	// Cancelled and filled orders must not have come back.
	if r.Cancel(4) || r.Cancel(6) {
		t.Fatal("dead orders resurrected by restore")
	}
}

func TestRestoreRejectsGarbage(t *testing.T) {
	if _, err := Restore(bytes.NewReader(nil)); !errors.Is(err, ErrBadSnapshot) {
		t.Fatalf("empty: %v", err)
	}
	e := NewEngine()
	e.Process(limit(1, Buy, 100, 1))
	e.Process(limit(2, Sell, 101, 1))
	var buf bytes.Buffer
	e.Snapshot(&buf)
	b := buf.Bytes()

	bad := append([]byte(nil), b...)
	bad[0] = 'X'
	if _, err := Restore(bytes.NewReader(bad)); !errors.Is(err, ErrBadSnapshot) {
		t.Fatalf("magic: %v", err)
	}
	if _, err := Restore(bytes.NewReader(b[:len(b)-1])); !errors.Is(err, ErrBadSnapshot) {
		t.Fatalf("truncated: %v", err)
	}
	crossed := append([]byte(nil), b...)
	crossed[snapHeaderSize+16] = 105 // bid price 105 > ask 101
	if _, err := Restore(bytes.NewReader(crossed)); !errors.Is(err, ErrBadSnapshot) {
		t.Fatalf("crossed: %v", err)
	}
}

// TestPropertySnapshotAtAnyPoint is the recovery proof: stop at a random
// point, snapshot, restore into a fresh engine, then feed both engines the
// rest of the stream. Every event and the final digest must be identical.
func TestPropertySnapshotAtAnyPoint(t *testing.T) {
	for seed := int64(0); seed < 40; seed++ {
		cmds := randomCommands(100+seed, 1500)
		cut := rand.New(rand.NewSource(seed)).Intn(len(cmds))

		live := NewEngine()
		for _, c := range cmds[:cut] {
			live.Apply(c)
		}
		var buf bytes.Buffer
		if err := live.Snapshot(&buf); err != nil {
			t.Fatal(err)
		}
		restored, err := Restore(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("seed %d cut %d: %v", seed, cut, err)
		}
		for i, c := range cmds[cut:] {
			a := append([]Event(nil), live.Apply(c)...)
			b := restored.Apply(c)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("seed %d cut %d op %d: %+v\nlive     %+v\nrestored %+v", seed, cut, i, c, a, b)
			}
		}
		if live.Digest() != restored.Digest() {
			t.Fatalf("seed %d cut %d: digest %x vs %x", seed, cut, live.Digest(), restored.Digest())
		}
	}
}

func TestEventKindStrings(t *testing.T) {
	kinds := []EventKind{EvAccepted, EvRejected, EvFill, EvRested, EvDiscarded, EvCancelled, EvCancelRejected}
	seen := map[string]bool{}
	for _, k := range kinds {
		s := k.String()
		if s == "" || seen[s] {
			t.Fatalf("kind %d string %q empty or duplicate", k, s)
		}
		seen[s] = true
	}
}
