// Package flow generates deterministic order flow for benchmarks.
//
// Two flows are provided. Stationary is the realistic one and the headline
// for every published number: the book hovers around a target depth the way
// a live book does, because most quotes are cancelled rather than filled.
// Growing is the original uniform-price flow kept as a stress case: about
// one order in eight rests forever, so the book grows without bound and the
// garbage collector's cost grows with it.
package flow

import (
	"math/rand"

	"github.com/Arman-Chaudhury/matchbook"
)

// Config shapes a stationary flow. The zero value is not useful; start from
// DefaultConfig.
type Config struct {
	Seed        int64
	Mid         int64   // starting mid price in ticks
	TargetDepth int     // resting orders the book hovers around
	MarketPct   float64 // share of submits that are market orders
	AggressPct  float64 // share of limit submits priced through the touch
	DriftPct    float64 // per-command chance the mid moves one tick
	Owners      int     // if > 0, submits get an Owner in 1..Owners
	STP         matchbook.STPMode
}

// DefaultConfig is the configuration behind every published number.
func DefaultConfig() Config {
	return Config{
		Seed:        42,
		Mid:         10_000,
		TargetDepth: 2_000,
		MarketPct:   0.03,
		AggressPct:  0.12,
		DriftPct:    0.02,
	}
}

// Stats describes a generated stream, so a report can say exactly what it
// measured.
type Stats struct {
	Submits, Markets, Cancels int
	FinalResting              int
	MaxResting                int
}

// Stationary returns n commands. It drives a private engine while it
// generates, so every cancel names an order that is resting at that moment;
// because the engine is deterministic, replaying the returned commands into
// a fresh engine configured with cfg.STP reproduces exactly the same book.
//
// Each step either cancels a random live order or submits a new one. The
// cancel probability rises above one half when the book is deeper than the
// target and falls below it when shallower, which holds depth near the
// target; fills also remove liquidity, so the book settles somewhat below
// it. Passive limit prices sit a geometric distance behind the mid, so most
// liquidity is near the touch with a thinning tail behind it.
func Stationary(cfg Config, n int) ([]matchbook.Command, Stats) {
	rng := rand.New(rand.NewSource(cfg.Seed))
	eng := matchbook.NewEngine(matchbook.WithSelfTradePrevention(cfg.STP))
	cmds := make([]matchbook.Command, 0, n)
	var st Stats

	live := newLiveSet()
	remaining := map[int64]int64{}
	mid := cfg.Mid
	var nextID int64

	track := func(evs []matchbook.Event) {
		for _, ev := range evs {
			switch ev.Kind {
			case matchbook.EvRested:
				remaining[ev.OrderID] = ev.Qty
				live.add(ev.OrderID)
			case matchbook.EvFill:
				if remaining[ev.MakerID] -= ev.Qty; remaining[ev.MakerID] == 0 {
					delete(remaining, ev.MakerID)
					live.remove(ev.MakerID)
				}
			case matchbook.EvCancelled:
				delete(remaining, ev.OrderID)
				live.remove(ev.OrderID)
			}
		}
	}

	for len(cmds) < n {
		if rng.Float64() < cfg.DriftPct {
			mid += int64(rng.Intn(3) - 1)
		}
		depth := live.len()
		cancelP := 0.5 * float64(depth) / float64(cfg.TargetDepth)
		if cancelP > 0.9 {
			cancelP = 0.9
		}
		if depth > 0 && rng.Float64() < cancelP {
			c := matchbook.Command{Kind: matchbook.CmdCancel, Order: matchbook.Order{ID: live.pick(rng)}}
			cmds = append(cmds, c)
			track(eng.Apply(c))
			st.Cancels++
			continue
		}

		nextID++
		o := matchbook.Order{
			ID:   nextID,
			Side: matchbook.Side(rng.Intn(2)),
			Type: matchbook.Limit,
			Qty:  int64(1 + rng.Intn(100)),
		}
		if cfg.Owners > 0 {
			o.Owner = int64(1 + rng.Intn(cfg.Owners))
		}
		switch r := rng.Float64(); {
		case r < cfg.MarketPct:
			o.Type = matchbook.Market
			st.Markets++
		case r < cfg.MarketPct+cfg.AggressPct:
			o.Price = priceAt(mid, o.Side, -int64(1+rng.Intn(3))) // through the touch
		default:
			o.Price = priceAt(mid, o.Side, geometric(rng, 0.25))
		}
		c := matchbook.Command{Kind: matchbook.CmdSubmit, Order: o}
		cmds = append(cmds, c)
		track(eng.Apply(c))
		st.Submits++
		if d := live.len(); d > st.MaxResting {
			st.MaxResting = d
		}
	}
	st.FinalResting = live.len()
	return cmds, st
}

// priceAt returns a limit price behind (positive) or through (negative)
// the mid by the given number of ticks, never below one tick.
func priceAt(mid int64, side matchbook.Side, behind int64) int64 {
	p := mid - 1 - behind
	if side == matchbook.Sell {
		p = mid + 1 + behind
	}
	if p < 1 {
		p = 1
	}
	return p
}

// geometric draws 0, 1, 2, ... with P(k) = p(1-p)^k.
func geometric(rng *rand.Rand, p float64) int64 {
	var k int64
	for rng.Float64() >= p {
		k++
	}
	return k
}

// Growing is the original benchmark flow: 90% limits uniform over a 40-tick
// band, 10% market orders, no cancels. IDs start at 1. The book grows by
// roughly one resting order per eight submitted.
func Growing(seed int64, n int) []matchbook.Order {
	rng := rand.New(rand.NewSource(seed))
	orders := make([]matchbook.Order, n)
	for i := range orders {
		o := matchbook.Order{ID: int64(i + 1), Side: matchbook.Side(rng.Intn(2)), Type: matchbook.Limit, Qty: int64(1 + rng.Intn(100))}
		if rng.Intn(10) == 0 {
			o.Type = matchbook.Market
		} else {
			o.Price = int64(1000 + rng.Intn(40))
		}
		orders[i] = o
	}
	return orders
}

// liveSet supports O(1) add, remove and uniform random pick.
type liveSet struct {
	ids []int64
	pos map[int64]int
}

func newLiveSet() *liveSet { return &liveSet{pos: map[int64]int{}} }

func (s *liveSet) len() int { return len(s.ids) }

func (s *liveSet) add(id int64) {
	s.pos[id] = len(s.ids)
	s.ids = append(s.ids, id)
}

func (s *liveSet) remove(id int64) {
	i, ok := s.pos[id]
	if !ok {
		return
	}
	last := s.ids[len(s.ids)-1]
	s.ids[i] = last
	s.pos[last] = i
	s.ids = s.ids[:len(s.ids)-1]
	delete(s.pos, id)
}

func (s *liveSet) pick(rng *rand.Rand) int64 { return s.ids[rng.Intn(len(s.ids))] }
