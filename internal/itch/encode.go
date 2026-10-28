package itch

import "encoding/binary"

// Encoders build messages byte for byte in the ITCH 5.0 layout. They exist
// so tests can construct exact feeds; the replay tool only reads.

func header(typ byte, n int, locate uint16, ts uint64) Msg {
	m := make(Msg, n)
	m[0] = typ
	binary.BigEndian.PutUint16(m[1:], locate)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], ts)
	copy(m[5:11], b[2:])
	return m
}

func putStock(b []byte, s string) {
	for i := 0; i < 8; i++ {
		b[i] = ' '
	}
	copy(b, s)
}

func EncodeSystemEvent(ts uint64, code byte) Msg {
	m := header(TypeSystemEvent, 12, 0, ts)
	m[11] = code
	return m
}

func EncodeStockDirectory(locate uint16, ts uint64, sym string) Msg {
	m := header(TypeStockDirectory, 39, locate, ts)
	putStock(m[11:], sym)
	m[19] = 'Q'
	m[20] = 'N'
	binary.BigEndian.PutUint32(m[21:], 100)
	return m
}

func EncodeTradingAction(locate uint16, ts uint64, sym string, state byte) Msg {
	m := header(TypeTradingAction, 25, locate, ts)
	putStock(m[11:], sym)
	m[19] = state
	return m
}

func EncodeAddOrder(locate uint16, ts, ref uint64, buy bool, shares uint32, sym string, price uint32) Msg {
	m := header(TypeAddOrder, 36, locate, ts)
	binary.BigEndian.PutUint64(m[11:], ref)
	m[19] = 'S'
	if buy {
		m[19] = 'B'
	}
	binary.BigEndian.PutUint32(m[20:], shares)
	putStock(m[24:], sym)
	binary.BigEndian.PutUint32(m[32:], price)
	return m
}

func EncodeExecuted(locate uint16, ts, ref uint64, shares uint32, match uint64) Msg {
	m := header(TypeOrderExecuted, 31, locate, ts)
	binary.BigEndian.PutUint64(m[11:], ref)
	binary.BigEndian.PutUint32(m[19:], shares)
	binary.BigEndian.PutUint64(m[23:], match)
	return m
}

func EncodeExecutedWithPrice(locate uint16, ts, ref uint64, shares uint32, match uint64, price uint32) Msg {
	m := header(TypeExecutedWithPrice, 36, locate, ts)
	binary.BigEndian.PutUint64(m[11:], ref)
	binary.BigEndian.PutUint32(m[19:], shares)
	binary.BigEndian.PutUint64(m[23:], match)
	m[31] = 'Y'
	binary.BigEndian.PutUint32(m[32:], price)
	return m
}

func EncodeOrderCancel(locate uint16, ts, ref uint64, shares uint32) Msg {
	m := header(TypeOrderCancel, 23, locate, ts)
	binary.BigEndian.PutUint64(m[11:], ref)
	binary.BigEndian.PutUint32(m[19:], shares)
	return m
}

func EncodeOrderDelete(locate uint16, ts, ref uint64) Msg {
	m := header(TypeOrderDelete, 19, locate, ts)
	binary.BigEndian.PutUint64(m[11:], ref)
	return m
}

func EncodeOrderReplace(locate uint16, ts, orig, newRef uint64, shares, price uint32) Msg {
	m := header(TypeOrderReplace, 35, locate, ts)
	binary.BigEndian.PutUint64(m[11:], orig)
	binary.BigEndian.PutUint64(m[19:], newRef)
	binary.BigEndian.PutUint32(m[27:], shares)
	binary.BigEndian.PutUint32(m[31:], price)
	return m
}
