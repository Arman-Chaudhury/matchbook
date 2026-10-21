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

// Status reports what Process did with an order.
type Status int8

const (
	Rejected  Status = iota // invalid input; the book is unchanged
	Filled                  // fully filled; nothing rests
	Rested                  // limit order: the unfilled remainder rests on the book
	Discarded               // market order: the unfilled remainder was dropped (IOC)
)

func (s Status) String() string {
	switch s {
	case Rejected:
		return "rejected"
	case Filled:
		return "filled"
	case Rested:
		return "rested"
	}
	return "discarded"
}

// Reject explains a Rejected result.
type Reject int8

const (
	RejectNone        Reject = iota
	RejectQty                // Qty <= 0
	RejectPrice              // limit order with Price <= 0
	RejectDuplicateID        // an order with this ID is already resting
)

// Result is the outcome of one call to Process.
//
// Trades is only valid until the next call to Process. Remaining is the
// quantity left after matching: resting on the book for Rested, discarded
// for Discarded, zero otherwise.
type Result struct {
	Status    Status
	Reject    Reject
	Trades    []Trade
	Remaining int64
}
