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

// TimeInForce says how long the unfilled part of a limit order may live.
// Market orders are always immediate-or-cancel and ignore this field.
type TimeInForce int8

const (
	GTC TimeInForce = iota // good till cancelled: the remainder rests
	IOC                    // immediate or cancel: the remainder is discarded
	FOK                    // fill or kill: fills completely or not at all
)

func (t TimeInForce) String() string {
	switch t {
	case GTC:
		return "GTC"
	case IOC:
		return "IOC"
	}
	return "FOK"
}

// STPMode is the engine's self-trade prevention policy, applied when an
// incoming order would trade against a resting order with the same
// non-zero Owner.
type STPMode int8

const (
	STPNone        STPMode = iota // owners may trade with themselves
	STPCancelTaker                // the incoming order's remainder is discarded
	STPCancelMaker                // the resting order is cancelled; matching continues
)

// Order is a request to trade. Prices are integer ticks to avoid float
// rounding in the matching path; Qty is in integer lots. Owner identifies
// the participant for self-trade prevention; zero means anonymous and is
// never prevented.
type Order struct {
	ID       int64
	Owner    int64
	Side     Side
	Type     Type
	TIF      TimeInForce
	PostOnly bool
	Price    int64
	Qty      int64
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
	RejectUnknownID          // Replace named an ID that is not resting
	RejectWouldCross         // a post-only order would have traded
	RejectTIF                // TIF out of range
	RejectSelfTrade          // reason on EvDiscarded/EvCancelled: self-trade prevention
	RejectReplaced           // reason on EvCancelled: the order was replaced and re-entered
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
