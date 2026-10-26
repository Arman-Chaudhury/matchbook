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
//	kind(1) side(1) type(1) pad(1) id(8) price(8) qty(8)
const (
	journalMagic = "MBJ1"
	recordSize   = 28
)

var (
	// ErrBadMagic means the reader was not positioned at a matchbook journal.
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
	b[0] = byte(c.Kind)
	b[1] = byte(c.Order.Side)
	b[2] = byte(c.Order.Type)
	b[3] = 0
	binary.LittleEndian.PutUint64(b[4:], uint64(c.Order.ID))
	binary.LittleEndian.PutUint64(b[12:], uint64(c.Order.Price))
	binary.LittleEndian.PutUint64(b[20:], uint64(c.Order.Qty))
}

func decodeCommand(b *[recordSize]byte) (Command, error) {
	c := Command{
		Kind: CommandKind(b[0]),
		Order: Order{
			Side:  Side(b[1]),
			Type:  Type(b[2]),
			ID:    int64(binary.LittleEndian.Uint64(b[4:])),
			Price: int64(binary.LittleEndian.Uint64(b[12:])),
			Qty:   int64(binary.LittleEndian.Uint64(b[20:])),
		},
	}
	if c.Kind > CmdCancel || c.Order.Side > Sell || c.Order.Type > Market || b[3] != 0 {
		return Command{}, fmt.Errorf("matchbook: corrupt journal record %x", b[:4])
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
