package matchbook

import (
	"bytes"
	"errors"
	"testing"
)

var errDisk = errors.New("disk full")

// failWriter accepts the first okWrites writes and fails every later one.
type failWriter struct {
	okWrites int
	buf      bytes.Buffer
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.okWrites == 0 {
		return 0, errDisk
	}
	f.okWrites--
	return f.buf.Write(p)
}

func TestWriteErrorsPropagate(t *testing.T) {
	if _, err := NewJournal(&failWriter{}); !errors.Is(err, errDisk) {
		t.Fatalf("NewJournal: %v", err)
	}
	j, err := NewJournal(&failWriter{okWrites: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Command{Kind: CmdSubmit, Order: limit(1, Buy, 1, 1)}); !errors.Is(err, errDisk) {
		t.Fatalf("Append: %v", err)
	}
	e := NewEngine()
	e.Process(limit(1, Buy, 1, 1))
	if err := e.Snapshot(&failWriter{}); !errors.Is(err, errDisk) {
		t.Fatalf("Snapshot: %v", err)
	}
}

// errReader fails after serving a prefix, to exercise non-EOF read errors.
type errReader struct{ data []byte }

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errDisk
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestReadErrorsPropagate(t *testing.T) {
	if _, err := Replay(&errReader{}, NewEngine()); !errors.Is(err, errDisk) {
		t.Fatalf("Replay header: %v", err)
	}
	if n, err := Replay(&errReader{data: []byte(journalMagic)}, NewEngine()); !errors.Is(err, errDisk) || n != 0 {
		t.Fatalf("Replay body: n=%d err=%v", n, err)
	}
}
