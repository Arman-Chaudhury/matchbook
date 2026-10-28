package itch

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestDecodeAddOrderFromSpecBytes decodes an Add Order written out byte by
// byte from the ITCH 5.0 layout, independent of the encoders, so a wrong
// offset in both encoder and decoder cannot hide.
func TestDecodeAddOrderFromSpecBytes(t *testing.T) {
	raw := []byte{
		0x00, 0x24, // length 36
		'A',        // message type
		0x00, 0x0d, // stock locate 13
		0x00, 0x00, // tracking number
		0x00, 0x00, 0x0d, 0xf8, 0x47, 0x58, // timestamp (6 bytes)
		0, 0, 0, 0, 0, 0, 0x30, 0x39, // order reference 12345
		'B',                    // buy
		0x00, 0x00, 0x01, 0x2c, // 300 shares
		'A', 'A', 'P', 'L', ' ', ' ', ' ', ' ', // stock
		0x00, 0x2d, 0xc6, 0xc0, // price 300.0000 = 3000000
	}
	r := NewReader(bytes.NewReader(raw))
	m, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if m.Type() != TypeAddOrder || m.Locate() != 13 {
		t.Fatalf("type %q locate %d", m.Type(), m.Locate())
	}
	if ts := m.Timestamp(); ts != 0x0df84758 {
		t.Fatalf("timestamp %#x", ts)
	}
	got := m.AddOrder()
	want := AddOrder{Ref: 12345, Buy: true, Shares: 300, Stock: "AAPL", Price: 3_000_000}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestRoundTripEveryDecodedType(t *testing.T) {
	const ts = 34_200_000_000_123 // 09:30:00.000000123
	msgs := []Msg{
		EncodeSystemEvent(ts, 'Q'),
		EncodeStockDirectory(7, ts, "MSFT"),
		EncodeTradingAction(7, ts, "MSFT", 'T'),
		EncodeAddOrder(7, ts, 1, false, 200, "MSFT", 1_575_000),
		EncodeExecuted(7, ts, 1, 50, 99),
		EncodeExecutedWithPrice(7, ts, 1, 25, 100, 1_574_900),
		EncodeOrderCancel(7, ts, 1, 10),
		EncodeOrderReplace(7, ts, 1, 2, 300, 1_575_100),
		EncodeOrderDelete(7, ts, 2),
	}
	var buf bytes.Buffer
	for _, m := range msgs {
		if err := WriteMsg(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(&buf)
	next := func() Msg {
		t.Helper()
		m, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if m.Timestamp() != ts {
			t.Fatalf("type %q timestamp %d", m.Type(), m.Timestamp())
		}
		return m
	}
	if e := next().SystemEvent(); e.Code != 'Q' {
		t.Fatalf("%+v", e)
	}
	if d := next().StockDirectory(); d.Stock != "MSFT" || d.RoundLotSize != 100 || d.MarketCategory != 'Q' {
		t.Fatalf("%+v", d)
	}
	if a := next().TradingAction(); a.Stock != "MSFT" || a.State != 'T' {
		t.Fatalf("%+v", a)
	}
	m := next()
	if m.Locate() != 7 {
		t.Fatalf("locate %d", m.Locate())
	}
	if a := m.AddOrder(); a != (AddOrder{Ref: 1, Buy: false, Shares: 200, Stock: "MSFT", Price: 1_575_000}) {
		t.Fatalf("%+v", a)
	}
	if e := next().Executed(); e != (Executed{Ref: 1, Shares: 50, Match: 99}) {
		t.Fatalf("%+v", e)
	}
	if e := next().ExecutedWithPrice(); e != (ExecutedWithPrice{Ref: 1, Shares: 25, Match: 100, Printable: true, Price: 1_574_900}) {
		t.Fatalf("%+v", e)
	}
	if c := next().OrderCancel(); c != (OrderCancel{Ref: 1, Shares: 10}) {
		t.Fatalf("%+v", c)
	}
	if u := next().OrderReplace(); u != (OrderReplace{Orig: 1, New: 2, Shares: 300, Price: 1_575_100}) {
		t.Fatalf("%+v", u)
	}
	if d := next().OrderDelete(); d != (OrderDelete{Ref: 2}) {
		t.Fatalf("%+v", d)
	}
	if r.Count() != uint64(len(msgs)) {
		t.Fatalf("count %d", r.Count())
	}
}

func TestUnknownTypesAreFramedAndPassedThrough(t *testing.T) {
	var buf bytes.Buffer
	odd := header('I', 50, 3, 1) // NOII, not decoded
	WriteMsg(&buf, odd)
	WriteMsg(&buf, EncodeOrderDelete(3, 2, 9))
	r := NewReader(&buf)
	m, err := r.Next()
	if err != nil || m.Type() != 'I' || len(m) != 50 {
		t.Fatalf("unknown type: %v %q %d", err, m.Type(), len(m))
	}
	if m, err := r.Next(); err != nil || m.OrderDelete().Ref != 9 {
		t.Fatalf("message after unknown type: %v", err)
	}
}

func TestMalformedStreams(t *testing.T) {
	// Wrong length for a decoded type.
	var buf bytes.Buffer
	WriteMsg(&buf, EncodeOrderDelete(1, 1, 1)[:18])
	if _, err := NewReader(&buf).Next(); !errors.Is(err, ErrShort) {
		t.Fatalf("short delete: %v", err)
	}
	// Torn in the middle of a message.
	buf.Reset()
	WriteMsg(&buf, EncodeOrderDelete(1, 1, 1))
	torn := buf.Bytes()[:buf.Len()-3]
	if _, err := NewReader(bytes.NewReader(torn)).Next(); err != io.ErrUnexpectedEOF {
		t.Fatalf("torn: %v", err)
	}
	// Length smaller than the header.
	if _, err := NewReader(bytes.NewReader([]byte{0, 3, 'A', 0, 0})).Next(); err == nil {
		t.Fatal("tiny length accepted")
	}
	// Torn inside the length prefix.
	if _, err := NewReader(bytes.NewReader([]byte{0})).Next(); err != io.ErrUnexpectedEOF {
		t.Fatalf("torn prefix: %v", err)
	}
}
