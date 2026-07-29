package matchbook

// Side of the book an order rests on or takes from.
type Side int8

const (
	Buy Side = iota
	Sell
)

func (s Side) String() string {
	if s == Buy {
		return "buy"
	}
	return "sell"
}

// Opposite returns the side an order matches against.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// Type distinguishes resting-capable limit orders from immediate market orders.
type Type int8

const (
	Limit Type = iota
	Market
)

// Order is a request to trade. Prices are integer ticks to avoid float
// rounding in the matching path; Qty is in integer lots.
type Order struct {
	ID    int64
	Side  Side
	Type  Type
	Price int64
	Qty   int64
}

// Trade is a single fill between a taker (incoming order) and a maker
// (resting order). Price is always the maker's price (price improvement
// goes to the taker).
type Trade struct {
	TakerID int64
	MakerID int64
	Price   int64
	Qty     int64
}
