// Command itchreplay replays a NASDAQ ITCH 5.0 feed through one matchbook
// engine per symbol and reports how the engine compared with the exchange.
// See internal/replay for what is checked.
//
//	go run ./cmd/itchreplay data/itch-20191230.itch.gz
//	go run ./cmd/itchreplay -symbols AAPL -v data/itch-20191230.itch.gz
package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Arman-Chaudhury/matchbook/internal/replay"
)

func main() {
	symbols := flag.String("symbols", "", "comma-separated symbols to replay (default: all in the file)")
	depthEvery := flag.Int("depth-every", 10_000, "compare full depth every N messages per symbol")
	verbose := flag.Bool("v", false, "print recorded mismatches")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: itchreplay [flags] FILE (.gz or raw ITCH)")
		os.Exit(2)
	}
	f, err := os.Open(flag.Arg(0))
	if err != nil {
		fail("%v", err)
	}
	defer f.Close()
	var r io.Reader = bufio.NewReaderSize(f, 1<<20)
	if strings.HasSuffix(flag.Arg(0), ".gz") {
		zr, err := gzip.NewReader(r)
		if err != nil {
			fail("%v", err)
		}
		r = zr
	}
	only := map[string]bool{}
	for _, s := range strings.Split(*symbols, ",") {
		if s = strings.TrimSpace(strings.ToUpper(s)); s != "" {
			only[s] = true
		}
	}

	start := time.Now()
	res, err := replay.Feed(r, only, replay.Options{DepthEvery: *depthEvery})
	if err != nil {
		fail("%v", err)
	}
	elapsed := time.Since(start)

	fmt.Printf("feed      %s: %d messages, replayed in %s\n", flag.Arg(0), res.Messages, elapsed.Round(time.Millisecond))
	if res.MarketOpen > 0 {
		fmt.Printf("session   market open %s, close %s\n", clock(res.MarketOpen), clock(res.MarketClose))
	}
	clean := true
	for _, s := range res.Symbols {
		st := s.Stats
		if st.Messages == 0 {
			continue
		}
		fmt.Printf("\n%s  %d messages\n", s.Name, st.Messages)
		fmt.Printf("  orders      %d adds, %d partial cancels, %d deletes, %d replaces\n", st.Adds, st.Cancels, st.Deletes, st.Replaces)
		fmt.Printf("  priority    %d of %d executions replayed through matching with the exact fill NASDAQ reported\n", st.ExecMatched, st.Executions)
		fmt.Printf("              %d not first in line, of which %d explained by NASDAQ's receipt-order priority\n",
			st.ExecNotFront, st.ExecReceiptOrder)
		fmt.Printf("              %d not at the best price, %d wrong fill, %d on refused orders\n",
			st.ExecNotBestPrice, st.ExecWrongFill, st.ExecOnUnplaced)
		fmt.Printf("  other       %d cross executions, %d hidden-order trades, %d cross trades, %d trading actions\n",
			st.ExecWithPrice, st.HiddenTrades, st.CrossTrades, st.TradingActions)
		fmt.Printf("  book        %d top-of-book checks, %d mismatches; %d full-depth checks, %d mismatches\n",
			st.TopChecks, st.TopMismatches, st.DepthChecks, st.DepthMismatch)
		fmt.Printf("  feed        %d adds would have crossed the book, %d unknown order references\n", st.AddsWouldCross, st.UnknownRef)
		fmt.Printf("  end         %d orders resting, engine digest %016x\n", s.RestingOrders(), s.Engine.Digest())
		fmt.Printf("  unexplained %d\n", st.Unexplained())
		if st.Unexplained() > 0 {
			clean = false
		}
		if *verbose {
			for _, m := range s.Mismatches() {
				fmt.Printf("    %s  %-20s %s\n", clock(m.Timestamp), m.Kind, m.Detail)
			}
		}
	}
	if !clean {
		fmt.Println("\nresult    unexplained disagreements with the feed; run with -v for details")
		os.Exit(1)
	}
	fmt.Println("\nresult    the book matched the feed after every message, and every execution")
	fmt.Println("          matched the engine's priority or NASDAQ's receipt-order priority")
}

// clock formats nanoseconds since midnight as HH:MM:SS.mmm.
func clock(ns uint64) string {
	d := time.Duration(ns)
	h, m := int(d.Hours()), int(d.Minutes())%60
	s := d.Seconds() - float64(h*3600+m*60)
	return fmt.Sprintf("%02d:%02d:%06.3f", h, m, s)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "itchreplay: "+format+"\n", args...)
	os.Exit(1)
}
