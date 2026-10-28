package replay

import (
	"compress/gzip"
	"os"
	"testing"
)

// realFeed is produced by cmd/itchextract from NASDAQ's public ITCH 5.0
// sample for 2019-12-30. It is not committed; see the README for how to
// fetch it. Without it this test is skipped.
const realFeed = "../../data/itch-20191230.itch.gz"

// TestRealNASDAQFeed replays a full trading day of real NASDAQ messages and
// requires that nothing is unexplained: the book matches after every
// message and every execution agrees with the engine's priority or with
// NASDAQ's receipt-order priority. It also replays twice and requires the
// same digest per symbol.
func TestRealNASDAQFeed(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	run := func() *Result {
		f, err := os.Open(realFeed)
		if err != nil {
			t.Skipf("no real feed (%v); run cmd/itchextract to fetch it", err)
		}
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Feed(zr, nil, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	a := run()
	executions := 0
	for _, s := range a.Symbols {
		if s.Stats.Messages == 0 {
			continue
		}
		executions += s.Stats.Executions
		if u := s.Stats.Unexplained(); u != 0 {
			t.Errorf("%s: %d unexplained: %+v\n%v", s.Name, u, s.Stats, s.Mismatches())
		}
		if s.RestingOrders() != 0 {
			t.Errorf("%s: %d orders still resting after the close", s.Name, s.RestingOrders())
		}
	}
	if executions == 0 {
		t.Fatal("feed contained no executions")
	}
	b := run()
	for i := range a.Symbols {
		if a.Symbols[i].Engine.Digest() != b.Symbols[i].Engine.Digest() {
			t.Fatalf("%s: replay is not deterministic", a.Symbols[i].Name)
		}
	}
}
