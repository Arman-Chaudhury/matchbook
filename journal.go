package matchbook

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The journal is the engine's write-ahead log of commands. Because the
// engine is deterministic, replaying the journal into a fresh engine
// reproduces every event and the same Digest. Records are fixed width so a
// torn tail (a crash mid-write) is detected as a short read rather than
// misparsed.
//
// Layout: 4-byte magic, then records of recordSize bytes, little-endian:
//
//	kind(1) side(1) type(1) tif(1) postonly(1) pad(3) id(8) owner(8) price(8) qty(8)
const (
	journalMagic = "MBJ2"
	recordSize   = 40
)

var (
	// ErrBadMagic means the reader was not positioned at a matchbook journal
	// of this version.
	ErrBadMagic = errors.New("matchbook: not a journal (bad magic)")
	// ErrTornRecord means the journal ended partway through a record. The
	// commands before it were replayed; the partial one was dropped.
	ErrTornRecord = errors.New("matchbook: journal ends mid-record")
)

// Journal appends commands to w. It performs one Write per command; wrap w
// in a bufio.Writer if that matters and remember to Flush.
type Journal struct {
	w   io.Writer
	buf [recordSize]byte
}

// NewJournal writes the header to w and returns a Journal ready to append.
func NewJournal(w io.Writer) (*Journal, error) {
	if _, err := io.WriteString(w, journalMagic); err != nil {
		return nil, err
	}
	return &Journal{w: w}, nil
}

// Append records one command.
func (j *Journal) Append(c Command) error {
	encodeCommand(&j.buf, c)
	_, err := j.w.Write(j.buf[:])
	return err
}

func encodeCommand(b *[recordSize]byte, c Command) {
	*b = [recordSize]byte{}
	b[0] = byte(c.Kind)
	b[1] = byte(c.Order.Side)
	b[2] = byte(c.Order.Type)
	b[3] = byte(c.Order.TIF)
	if c.Order.PostOnly {
		b[4] = 1
	}
	binary.LittleEndian.PutUint64(b[8:], uint64(c.Order.ID))
	binary.LittleEndian.PutUint64(b[16:], uint64(c.Order.Owner))
	binary.LittleEndian.PutUint64(b[24:], uint64(c.Order.Price))
	binary.LittleEndian.PutUint64(b[32:], uint64(c.Order.Qty))
}

func decodeCommand(b *[recordSize]byte) (Command, error) {
	c := Command{
		Kind: CommandKind(b[0]),
		Order: Order{
			Side:     Side(b[1]),
			Type:     Type(b[2]),
			TIF:      TimeInForce(b[3]),
			PostOnly: b[4] == 1,
			ID:       int64(binary.LittleEndian.Uint64(b[8:])),
			Owner:    int64(binary.LittleEndian.Uint64(b[16:])),
			Price:    int64(binary.LittleEndian.Uint64(b[24:])),
			Qty:      int64(binary.LittleEndian.Uint64(b[32:])),
		},
	}
	if c.Kind > CmdReplace || c.Order.Side > Sell || c.Order.Type > Market ||
		b[4] > 1 || b[5] != 0 || b[6] != 0 || b[7] != 0 {
		return Command{}, fmt.Errorf("matchbook: corrupt journal record %x", b[:8])
	}
	return c, nil
}

// Replay reads a journal from r and applies every command to eng in order.
// It returns the number of commands applied. A journal that ends partway
// through a record returns ErrTornRecord after applying the whole records
// before it, which is the expected shape of a crash-interrupted log.
func Replay(r io.Reader, eng *Engine) (int, error) {
	var magic [len(journalMagic)]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return 0, ErrBadMagic
		}
		return 0, err
	}
	if string(magic[:]) != journalMagic {
		return 0, ErrBadMagic
	}
	var buf [recordSize]byte
	n := 0
	for {
		_, err := io.ReadFull(r, buf[:])
		switch err {
		case nil:
		case io.EOF:
			return n, nil
		case io.ErrUnexpectedEOF:
			return n, ErrTornRecord
		default:
			return n, err
		}
		c, err := decodeCommand(&buf)
		if err != nil {
			return n, err
		}
		eng.Apply(c)
		n++
	}
}
