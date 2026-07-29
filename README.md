# matchbook

A price-time priority limit order book and matching engine in Go. Single
instrument, single goroutine, zero locks — determinism is the point: the same
order stream always produces the same trade stream, which is what makes an
exchange core testable at all.

## Features

- **Limit and market orders** with price-time (FIFO) priority and partial fills
- **Price improvement to the taker** — fills always execute at the maker's price
- **O(1) cancels** via tombstoning with lazy cleanup at the queue head
- **Aggregated depth snapshots** (top-N levels per side)
- **Allocation-conscious hot path** — the trade buffer is reused across calls;
  integer tick prices keep floats out of the matching path entirely

## Architecture

```mermaid
flowchart LR
    subgraph Engine [single-goroutine engine]
        P[Process / Cancel] --> M{crosses?}
        M -- yes --> X[match loop\nbest level, FIFO head]
        M -- no --> R[rest on book]
        X --> T[trades out]
    end
    subgraph Book [order book]
        B[bids: max-heap of price levels]
        A[asks: min-heap of price levels]
        L["level = FIFO queue + live qty"]
        B --> L
        A --> L
    end
    P -.-> B
    P -.-> A
```

Each side of the book is a hash map of price → level plus a binary heap of
prices for best-price access. Levels are FIFO queues with an advancing head
index, so dequeue never reallocates. Emptied levels and cancelled orders are
pruned lazily when they surface at the top — cancels stay O(1) and the heap
never needs random deletion.

Scaling out is by sharding: one engine goroutine per instrument, orders routed
by symbol. Nothing in the core needs a mutex.

## Benchmarks

`go run ./cmd/bench -n 2000000` on an 8-core Intel Mac (go1.26.5), seeded
random flow (90% limit / 10% market, narrow price band for heavy crossing):

| metric | value |
|---|---|
| sustained throughput | **2.08M orders/sec** |
| latency p50 | 0.30 µs |
| latency p95 | 0.69 µs |
| latency p99 | 1.14 µs |
| trades generated | 1.72M from 2M orders |

Reproduce with a fixed seed: the flow generator is deterministic (`-seed`).

## Testing

- **Property-based suite**: 300 random scenarios × 400 operations, each
  cross-checked operation-by-operation against a naive O(n) reference
  implementation — trades, cancels, top-of-book, and final depth must be
  identical. The book-never-crossed invariant is asserted after every op.
- **Conservation property**: every unit of submitted quantity is provably
  filled, resting, cancelled, or discarded (market IOC remainder).
- Table-driven unit tests for FIFO priority, price priority, partial fills,
  price improvement, IOC semantics, cancels, and input validation.
- `go test -race -cover ./...` → **96.8% statement coverage**, race-clean.

## Usage

```go
eng := matchbook.NewEngine()

eng.Process(matchbook.Order{ID: 1, Side: matchbook.Sell, Type: matchbook.Limit, Price: 100, Qty: 10})
trades := eng.Process(matchbook.Order{ID: 2, Side: matchbook.Buy, Type: matchbook.Limit, Price: 102, Qty: 4})
// trades: [{TakerID:2 MakerID:1 Price:100 Qty:4}] — taker gets price improvement

bid, qty, ok := eng.BestBid()
depth := eng.Depth(matchbook.Sell, 5)
eng.Cancel(1)
```

## License

MIT
