package flow

import (
	"reflect"
	"testing"

	"github.com/Arman-Chaudhury/matchbook"
)

func TestStationaryReplaysExactly(t *testing.T) {
	cfg := DefaultConfig()
	a, sa := Stationary(cfg, 200_000)
	b, sb := Stationary(cfg, 200_000)
	if !reflect.DeepEqual(a, b) || sa != sb {
		t.Fatal("same config must generate the same stream")
	}

	e := matchbook.NewEngine()
	resting, minRest, maxRest := 0, 1<<30, 0
	for i, c := range a {
		for _, ev := range e.Apply(c) {
			switch ev.Kind {
			case matchbook.EvCancelRejected:
				t.Fatalf("command %d: cancel of %d missed; every cancel must target a live order", i, ev.OrderID)
			case matchbook.EvRejected:
				t.Fatalf("command %d: order rejected: %+v", i, ev)
			}
		}
		if i > 20_000 && i%1000 == 0 { // after warm-up
			resting = 0
			for _, side := range []matchbook.Side{matchbook.Buy, matchbook.Sell} {
				for _, d := range e.Depth(side, 1<<20) {
					resting += int(d.Qty)
				}
			}
			minRest, maxRest = min(minRest, resting), max(maxRest, resting)
		}
	}
	// The book is measured in quantity here (orders average ~50 lots).
	t.Logf("stats %+v; resting qty after warm-up ranged %d..%d", sa, minRest, maxRest)
	if sa.FinalResting < cfg.TargetDepth/2 || sa.FinalResting > cfg.TargetDepth*2 {
		t.Fatalf("final depth %d strayed from target %d", sa.FinalResting, cfg.TargetDepth)
	}
	if sa.MaxResting > cfg.TargetDepth*3 {
		t.Fatalf("depth peaked at %d, target %d: the flow is not stationary", sa.MaxResting, cfg.TargetDepth)
	}
	if sa.Cancels < len(a)/4 {
		t.Fatalf("only %d cancels in %d commands", sa.Cancels, len(a))
	}
}

func TestStationaryWithOwnersAndSTP(t *testing.T) {
	for _, mode := range []matchbook.STPMode{matchbook.STPCancelTaker, matchbook.STPCancelMaker} {
		cfg := DefaultConfig()
		cfg.Owners, cfg.STP = 8, mode
		cmds, st := Stationary(cfg, 100_000)
		e := matchbook.NewEngine(matchbook.WithSelfTradePrevention(mode))
		selfStops := 0
		for i, c := range cmds {
			if c.Kind == matchbook.CmdSubmit && (c.Order.Owner < 1 || c.Order.Owner > 8) {
				t.Fatalf("command %d: owner %d out of range", i, c.Order.Owner)
			}
			for _, ev := range e.Apply(c) {
				if ev.Kind == matchbook.EvCancelRejected {
					t.Fatalf("stp %d command %d: cancel missed", mode, i)
				}
				if ev.Reject == matchbook.RejectSelfTrade {
					selfStops++
				}
			}
		}
		if selfStops == 0 {
			t.Fatalf("stp %d: no self-trades were prevented, so the benchmark would not exercise STP", mode)
		}
		if st.MaxResting > cfg.TargetDepth*3 {
			t.Fatalf("stp %d: depth peaked at %d", mode, st.MaxResting)
		}
	}
}

func TestGrowingMatchesOriginalBenchFlow(t *testing.T) {
	o := Growing(42, 3)
	if o[0].ID != 1 || o[2].ID != 3 {
		t.Fatalf("ids %+v", o)
	}
	if !reflect.DeepEqual(Growing(42, 1000), Growing(42, 1000)) {
		t.Fatal("not deterministic")
	}
}

func TestLiveSet(t *testing.T) {
	s := newLiveSet()
	for i := int64(1); i <= 5; i++ {
		s.add(i)
	}
	s.remove(3)
	s.remove(3)
	s.remove(99)
	if s.len() != 4 {
		t.Fatalf("len %d", s.len())
	}
	for _, id := range s.ids {
		if s.ids[s.pos[id]] != id || id == 3 {
			t.Fatalf("index broken: %v %v", s.ids, s.pos)
		}
	}
	if p := priceAt(1, matchbook.Buy, 5); p != 1 {
		t.Fatalf("prices must floor at one tick, got %d", p)
	}
}
