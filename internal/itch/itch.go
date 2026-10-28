// Package itch reads NASDAQ TotalView-ITCH 5.0 binary feeds, the format
// NASDAQ publishes its full-depth order book in and the source behind
// datasets such as LOBSTER.
//
// A feed file is a sequence of messages, each preceded by a two-byte
// big-endian length. Every message starts with an 11-byte header: a
// one-byte type, a two-byte stock locate code, a two-byte tracking number
// and a six-byte timestamp in nanoseconds since midnight. All integers are
// big-endian and prices are fixed point with four implied decimals.
//
// Only the messages that affect the displayed order book, plus the stock
// directory and system events, are decoded. Every other message type is
// still framed and returned, so it can be skipped or copied through.
//
// Reference: NASDAQ TotalView-ITCH 5.0 specification.
package itch

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Message types decoded by this package.
const (
	TypeSystemEvent       = 'S'
	TypeStockDirectory    = 'R'
	TypeTradingAction     = 'H'
	TypeAddOrder          = 'A'
	TypeAddOrderMPID      = 'F'
	TypeOrderExecuted     = 'E'
	TypeExecutedWithPrice = 'C'
	TypeOrderCancel       = 'X'
	TypeOrderDelete       = 'D'
	TypeOrderReplace      = 'U'
	TypeTrade             = 'P'
	TypeCrossTrade        = 'Q'
)

// msgLen is the fixed length of each decoded message type.
var msgLen = map[byte]int{
	TypeSystemEvent:       12,
	TypeStockDirectory:    39,
	TypeTradingAction:     25,
	TypeAddOrder:          36,
	TypeAddOrderMPID:      40,
	TypeOrderExecuted:     31,
	TypeExecutedWithPrice: 36,
	TypeOrderCancel:       23,
	TypeOrderDelete:       19,
	TypeOrderReplace:      35,
	TypeTrade:             44,
	TypeCrossTrade:        40,
}

const headerLen = 11

// ErrShort means a decoded message type arrived with the wrong length.
var ErrShort = errors.New("itch: message has the wrong length for its type")

// Msg is one framed message. It aliases the reader's buffer and is only
// valid until the next call to Next.
type Msg []byte

func (m Msg) Type() byte { return m[0] }

// Locate is the stock locate code: the per-day integer NASDAQ assigns each
// symbol in its stock directory message.
func (m Msg) Locate() uint16 { return binary.BigEndian.Uint16(m[1:]) }

// Timestamp is nanoseconds since midnight.
func (m Msg) Timestamp() uint64 {
	var b [8]byte
	copy(b[2:], m[5:11])
	return binary.BigEndian.Uint64(b[:])
}

func (m Msg) check() error {
	if want, ok := msgLen[m.Type()]; ok && len(m) != want {
		return fmt.Errorf("%w: type %q has %d bytes, want %d", ErrShort, m.Type(), len(m), want)
	}
	return nil
}

// Reader frames messages from an ITCH byte stream.
type Reader struct {
	r   *bufio.Reader
	buf []byte
	n   uint64
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 1<<20), buf: make([]byte, 0, 64)}
}

// Count is the number of messages returned so far.
func (r *Reader) Count() uint64 { return r.n }

// Next returns the next message, io.EOF at a clean end of stream, or
// io.ErrUnexpectedEOF if the stream stops partway through a message.
func (r *Reader) Next() (Msg, error) {
	var lb [2]byte
	if _, err := io.ReadFull(r.r, lb[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lb[:]))
	if n < headerLen {
		return nil, fmt.Errorf("itch: %d-byte message after %d messages is shorter than the header", n, r.n)
	}
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	}
	r.buf = r.buf[:n]
	if _, err := io.ReadFull(r.r, r.buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	m := Msg(r.buf)
	if err := m.check(); err != nil {
		return nil, err
	}
	r.n++
	return m, nil
}

// WriteMsg writes m with its length prefix, the same framing Reader reads.
func WriteMsg(w io.Writer, m Msg) error {
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(len(m)))
	if _, err := w.Write(lb[:]); err != nil {
		return err
	}
	_, err := w.Write(m)
	return err
}

func u32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func u64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
func stock(b []byte) string {
	return strings.TrimRight(string(b[:8]), " ")
}

// SystemEvent marks session boundaries: 'O' start of messages, 'S' start
// of system hours, 'Q' start of market hours, 'M' end of market hours,
// 'E' end of system hours, 'C' end of messages.
type SystemEvent struct{ Code byte }

func (m Msg) SystemEvent() SystemEvent { return SystemEvent{Code: m[11]} }

// StockDirectory names the symbol behind a locate code.
type StockDirectory struct {
	Stock          string
	RoundLotSize   uint32
	MarketCategory byte
}

func (m Msg) StockDirectory() StockDirectory {
	return StockDirectory{Stock: stock(m[11:]), MarketCategory: m[19], RoundLotSize: u32(m[21:])}
}

// TradingAction reports a halt ('H'), pause ('P'), quotation-only period
// ('Q') or normal trading ('T') for a symbol.
type TradingAction struct {
	Stock string
	State byte
}

func (m Msg) TradingAction() TradingAction {
	return TradingAction{Stock: stock(m[11:]), State: m[19]}
}

// AddOrder is a new displayed order resting on the book. 'F' messages
// carry a market participant ID as well; it is not decoded.
type AddOrder struct {
	Ref    uint64
	Buy    bool
	Shares uint32
	Stock  string
	Price  uint32 // four implied decimals
}

func (m Msg) AddOrder() AddOrder {
	return AddOrder{Ref: u64(m[11:]), Buy: m[19] == 'B', Shares: u32(m[20:]), Stock: stock(m[24:]), Price: u32(m[32:])}
}

// Executed reports that Shares of resting order Ref traded at its own
// price against an incoming order.
type Executed struct {
	Ref    uint64
	Shares uint32
	Match  uint64
}

func (m Msg) Executed() Executed {
	return Executed{Ref: u64(m[11:]), Shares: u32(m[19:]), Match: u64(m[23:])}
}

// ExecutedWithPrice reports an execution at a price other than the order's
// own, as happens in the opening and closing crosses.
type ExecutedWithPrice struct {
	Ref       uint64
	Shares    uint32
	Match     uint64
	Printable bool
	Price     uint32
}

func (m Msg) ExecutedWithPrice() ExecutedWithPrice {
	return ExecutedWithPrice{Ref: u64(m[11:]), Shares: u32(m[19:]), Match: u64(m[23:]), Printable: m[31] == 'Y', Price: u32(m[32:])}
}

// OrderCancel removes Shares from a resting order without removing it.
type OrderCancel struct {
	Ref    uint64
	Shares uint32
}

func (m Msg) OrderCancel() OrderCancel { return OrderCancel{Ref: u64(m[11:]), Shares: u32(m[19:])} }

// OrderDelete removes a resting order entirely.
type OrderDelete struct{ Ref uint64 }

func (m Msg) OrderDelete() OrderDelete { return OrderDelete{Ref: u64(m[11:])} }

// OrderReplace cancels Orig and adds New with a new size and price on the
// same side. The new order loses time priority.
type OrderReplace struct {
	Orig, New uint64
	Shares    uint32
	Price     uint32
}

func (m Msg) OrderReplace() OrderReplace {
	return OrderReplace{Orig: u64(m[11:]), New: u64(m[19:]), Shares: u32(m[27:]), Price: u32(m[31:])}
}
