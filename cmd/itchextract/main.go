// Command itchextract streams a gzipped NASDAQ ITCH 5.0 day file and keeps
// only the messages for chosen symbols, plus system events. A full day is
// several gigabytes compressed; the extract for a handful of symbols is a
// few tens of megabytes, small enough to replay repeatedly.
//
// It reads from standard input so the day file never has to be stored:
//
//	curl -s 'https://emi.nasdaq.com/ITCH/Nasdaq%20ITCH/12302019.NASDAQ_ITCH50.gz' |
//	  go run ./cmd/itchextract -symbols AAPL,MSFT -o data/itch-20191230.itch.gz
//
// The SHA-256 of the compressed input is printed so a run can be tied to an
// exact file. gzip's own CRC-32 is verified at the end of the stream, so a
// corrupted or truncated download fails instead of producing a short
// extract.
package main

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Arman-Chaudhury/matchbook/internal/itch"
)

func main() {
	symbols := flag.String("symbols", "AAPL,MSFT", "comma-separated symbols to keep")
	out := flag.String("o", "", "output file (gzipped ITCH)")
	flag.Parse()
	if *out == "" {
		fail("-o is required")
	}
	want := map[string]bool{}
	for _, s := range strings.Split(*symbols, ",") {
		if s = strings.TrimSpace(strings.ToUpper(s)); s != "" {
			want[s] = true
		}
	}

	hash := sha256.New()
	counted := &countingReader{r: io.TeeReader(bufio.NewReaderSize(os.Stdin, 1<<20), hash)}
	zr, err := gzip.NewReader(counted)
	if err != nil {
		fail("not a gzip stream: %v", err)
	}
	f, err := os.Create(*out)
	if err != nil {
		fail("%v", err)
	}
	zw := gzip.NewWriter(f)
	bw := bufio.NewWriterSize(zw, 1<<20)

	r := itch.NewReader(zr)
	keep := map[uint16]string{} // locate -> symbol
	kept := map[string]map[byte]int{}
	start := time.Now()
	lastReport := start
	for {
		m, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail("after %d messages: %v", r.Count(), err)
		}
		switch {
		case m.Type() == itch.TypeStockDirectory:
			sym := m.StockDirectory().Stock
			if !want[sym] {
				continue
			}
			keep[m.Locate()] = sym
			kept[sym] = map[byte]int{}
		case m.Type() == itch.TypeSystemEvent:
		default:
			if _, ok := keep[m.Locate()]; !ok {
				continue
			}
		}
		if sym, ok := keep[m.Locate()]; ok {
			kept[sym][m.Type()]++
		}
		if err := itch.WriteMsg(bw, m); err != nil {
			fail("%v", err)
		}
		if time.Since(lastReport) > 10*time.Second {
			lastReport = time.Now()
			fmt.Fprintf(os.Stderr, "  %.2f GB read, %d messages, %s\n",
				float64(counted.n)/1e9, r.Count(), time.Since(start).Round(time.Second))
		}
	}
	// Drain anything after the last message so the gzip trailer, and its
	// CRC-32 check, is read; then hash the rest of stdin as well.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		fail("gzip trailer: %v", err)
	}
	if err := zr.Close(); err != nil {
		fail("gzip: %v", err)
	}
	io.Copy(io.Discard, counted)
	if err := bw.Flush(); err != nil {
		fail("%v", err)
	}
	if err := zw.Close(); err != nil {
		fail("%v", err)
	}
	if err := f.Close(); err != nil {
		fail("%v", err)
	}

	fmt.Printf("input     %d bytes compressed, %d messages, sha256 %s\n",
		counted.n, r.Count(), hex.EncodeToString(hash.Sum(nil)))
	fmt.Printf("elapsed   %s\n", time.Since(start).Round(time.Second))
	syms := make([]string, 0, len(kept))
	for s := range kept {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	for _, s := range syms {
		total := 0
		var parts []string
		for _, t := range []byte("AFEXDUCPQH") {
			if n := kept[s][t]; n > 0 {
				parts = append(parts, fmt.Sprintf("%c=%d", t, n))
				total += n
			}
		}
		fmt.Printf("%-8s  %d book messages (%s)\n", s, total, strings.Join(parts, " "))
	}
	for s := range want {
		if _, ok := kept[s]; !ok {
			fmt.Printf("%-8s  not in the stock directory\n", s)
		}
	}
	fmt.Printf("wrote     %s\n", *out)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "itchextract: "+format+"\n", args...)
	os.Exit(1)
}
