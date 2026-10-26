package matchbook

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// A snapshot is the engine's complete state at an instant: every resting
// order with its arrival sequence, plus the counters and digest. Restoring
// a snapshot and then replaying the journal records written after it
// produces an engine indistinguishable from one that never stopped, which
// is how an exchange recovers without replaying its whole history.
//
// Layout, little-endian: 4-byte magic, evSeq(8), seq(8), digest(8),
// count(8), then count records of snapRecordSize bytes:
//
//	id(8) side(1) pad(7) price(8) qty(8) seq(8)
const (
	snapshotMagic  = "MBS1"
	snapHeaderSize = 4 + 8*4
	snapRecordSize = 40
)

// ErrBadSnapshot means r did not hold a well-formed matchbook snapshot.
var ErrBadSnapshot = errors.New("matchbook: malformed snapshot")

// Snapshot writes the engine's full state to w.
func (e *Engine) Snapshot(w io.Writer) error {
	live := make([]*resting, 0, len(e.byID))
	for _, r := range e.byID {
		live = append(live, r)
	}
	// Arrival order is the FIFO priority within a level; it must survive.
	sort.Slice(live, func(i, j int) bool { return live[i].seq < live[j].seq })

	buf := make([]byte, snapHeaderSize+snapRecordSize*len(live))
	copy(buf, snapshotMagic)
	binary.LittleEndian.PutUint64(buf[4:], e.evSeq)
	binary.LittleEndian.PutUint64(buf[12:], e.seq)
	binary.LittleEndian.PutUint64(buf[20:], e.digest)
	binary.LittleEndian.PutUint64(buf[28:], uint64(len(live)))
	off := snapHeaderSize
	for _, r := range live {
		binary.LittleEndian.PutUint64(buf[off:], uint64(r.id))
		buf[off+8] = byte(r.side)
		binary.LittleEndian.PutUint64(buf[off+16:], uint64(r.price))
		binary.LittleEndian.PutUint64(buf[off+24:], uint64(r.qty))
		binary.LittleEndian.PutUint64(buf[off+32:], r.seq)
		off += snapRecordSize
	}
	_, err := w.Write(buf)
	return err
}

// Restore builds an engine from a snapshot written by Snapshot.
func Restore(r io.Reader) (*Engine, error) {
	var hdr [snapHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrBadSnapshot, err)
	}
	if string(hdr[:4]) != snapshotMagic {
		return nil, ErrBadSnapshot
	}
	e := NewEngine()
	e.evSeq = binary.LittleEndian.Uint64(hdr[4:])
	e.seq = binary.LittleEndian.Uint64(hdr[12:])
	e.digest = binary.LittleEndian.Uint64(hdr[20:])
	count := binary.LittleEndian.Uint64(hdr[28:])

	var rec [snapRecordSize]byte
	var lastSeq uint64
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			return nil, fmt.Errorf("%w: record %d: %v", ErrBadSnapshot, i, err)
		}
		o := &resting{
			id:    int64(binary.LittleEndian.Uint64(rec[0:])),
			side:  Side(rec[8]),
			price: int64(binary.LittleEndian.Uint64(rec[16:])),
			qty:   int64(binary.LittleEndian.Uint64(rec[24:])),
			seq:   binary.LittleEndian.Uint64(rec[32:]),
		}
		switch {
		case o.side > Sell, o.price <= 0, o.qty <= 0,
			o.seq <= lastSeq, o.seq > e.seq:
			return nil, fmt.Errorf("%w: record %d invalid", ErrBadSnapshot, i)
		}
		if _, dup := e.byID[o.id]; dup {
			return nil, fmt.Errorf("%w: duplicate id %d", ErrBadSnapshot, o.id)
		}
		lastSeq = o.seq
		e.byID[o.id] = o
		if o.side == Buy {
			e.bids.add(o)
		} else {
			e.asks.add(o)
		}
	}
	if bid, _, ok := e.BestBid(); ok {
		if ask, _, ok := e.BestAsk(); ok && bid >= ask {
			return nil, fmt.Errorf("%w: crossed book", ErrBadSnapshot)
		}
	}
	return e, nil
}
