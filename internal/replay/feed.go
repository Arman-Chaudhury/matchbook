package replay

import (
	"io"

	"github.com/Arman-Chaudhury/matchbook/internal/itch"
)

// Result is a whole-feed replay.
type Result struct {
	Symbols                 []*Symbol // in stock-directory order
	Messages                uint64    // every framed message read, including skipped ones
	MarketOpen, MarketClose uint64    // timestamps of system events 'Q' and 'M', if seen
}

// Feed replays an ITCH stream. Symbols come from the stock directory; if
// only is non-empty, other symbols are skipped. A final depth check runs
// on every symbol at the end.
func Feed(r io.Reader, only map[string]bool, opt Options) (*Result, error) {
	ir := itch.NewReader(r)
	res := &Result{}
	byLocate := map[uint16]*Symbol{}
	var last uint64
	for {
		m, err := ir.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, err
		}
		res.Messages++
		last = m.Timestamp()
		switch m.Type() {
		case itch.TypeSystemEvent:
			switch m.SystemEvent().Code {
			case 'Q':
				res.MarketOpen = last
			case 'M':
				res.MarketClose = last
			}
			continue
		case itch.TypeStockDirectory:
			name := m.StockDirectory().Stock
			if len(only) == 0 || only[name] {
				s := NewSymbol(name, opt)
				byLocate[m.Locate()] = s
				res.Symbols = append(res.Symbols, s)
			}
			continue
		}
		if s, ok := byLocate[m.Locate()]; ok {
			s.Apply(m)
		}
	}
	for _, s := range res.Symbols {
		s.CheckDepth(last)
	}
	return res, nil
}
