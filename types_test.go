package matchbook

import "testing"

func TestStringers(t *testing.T) {
	if Buy.String() != "buy" || Sell.String() != "sell" {
		t.Fatalf("side strings: %q %q", Buy, Sell)
	}
	if Buy.Opposite() != Sell || Sell.Opposite() != Buy {
		t.Fatal("opposite sides")
	}
	want := map[Status]string{Rejected: "rejected", Filled: "filled", Rested: "rested", Discarded: "discarded"}
	for s, w := range want {
		if s.String() != w {
			t.Fatalf("status %d string = %q, want %q", s, s, w)
		}
	}
}
