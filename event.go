package matchbook

// EventKind classifies an entry in the engine's output stream.
type EventKind int8

const (
	EvAccepted       EventKind = iota // a valid order entered the engine
	EvRejected                        // an order was refused; Reject says why
	EvFill                            // taker OrderID traded Qty with MakerID at Price
	EvRested                          // the unfilled remainder of OrderID now rests at Price
	EvDiscarded                       // the unfilled remainder of a market order was dropped
	EvCancelled                       // OrderID left the book with Qty still unfilled
	EvCancelRejected                  // a cancel named an ID that was not resting
	EvReduced                         // OrderID shrank in place to Qty, keeping priority
)

func (k EventKind) String() string {
	switch k {
	case EvAccepted:
		return "accepted"
	case EvRejected:
		return "rejected"
	case EvFill:
		return "fill"
	case EvRested:
		return "rested"
	case EvDiscarded:
		return "discarded"
	case EvCancelled:
		return "cancelled"
	case EvReduced:
		return "reduced"
	}
	return "cancel_rejected"
}

// Event is one fact the engine emitted. The full event stream is enough to
// rebuild the book and every trade, and Seq numbers it without gaps, so a
// downstream consumer can detect loss. Which fields are meaningful depends
// on Kind:
//
//	EvAccepted        OrderID Side Type Price Qty (Qty is the submitted size)
//	EvRejected        OrderID Reject
//	EvFill            OrderID (taker) MakerID Price Qty
//	EvRested          OrderID Side Price Qty (Qty is what rests)
//	EvDiscarded       OrderID Qty; Reject is RejectSelfTrade when STP stopped it
//	EvCancelled       OrderID Side Price Qty (Qty is what was still unfilled);
//	                  Reject is RejectSelfTrade or RejectReplaced when the
//	                  engine, not the owner, removed it
//	EvCancelRejected  OrderID
//	EvReduced         OrderID Side Price Qty (Qty is the new resting size)
type Event struct {
	Seq     uint64
	Kind    EventKind
	Side    Side
	Type    Type
	Reject  Reject
	OrderID int64
	MakerID int64
	Price   int64
	Qty     int64
}

// The digest is an FNV-style multiply-xor chain applied one 64-bit word at
// a time rather than one byte at a time. It is a change detector for
// deterministic replay, not a cryptographic hash, and it runs on the hot
// path, so it is kept to one xor and one multiply per field.
const (
	fnv64Offset = 14695981039346656037
	fnv64Prime  = 1099511628211
)

// fold mixes one 64-bit value into the digest.
func fold(h uint64, v uint64) uint64 {
	return (h ^ v) * fnv64Prime
}

// digest folds every field of ev into h. Seq is included so that the same
// events in a different order, or with a gap, produce a different digest.
func (ev Event) digest(h uint64) uint64 {
	h = fold(h, ev.Seq)
	h = fold(h, uint64(ev.Kind)|uint64(ev.Side)<<8|uint64(ev.Type)<<16|uint64(ev.Reject)<<24)
	h = fold(h, uint64(ev.OrderID))
	h = fold(h, uint64(ev.MakerID))
	h = fold(h, uint64(ev.Price))
	h = fold(h, uint64(ev.Qty))
	return h
}

// CommandKind distinguishes the two inputs an engine accepts.
type CommandKind int8

const (
	CmdSubmit CommandKind = iota
	CmdCancel
	CmdReplace
)

// Command is one engine input. The engine is a deterministic function of
// its command stream, so a journal of commands is a complete record of
// everything it ever did. For CmdCancel only Order.ID is used; for
// CmdReplace, Order.ID, Order.Price and Order.Qty.
type Command struct {
	Kind  CommandKind
	Order Order
}
