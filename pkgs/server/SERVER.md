# Server Instance — Concurrency & Design Notes

> Reference document for `pkgs/server` and `cmd/server`.
> Read alongside `notes/CONCURRENCY.md` (client side).

---

## 1. The Context Tree

```
context.Background()
    └── context.WithCancel()         → rootCtx  (owned by main)
            └── context.WithValue()  → rootCtx  (*Config injected under ConfigKey)
                    ├── server.Run(rootCtx, stats)          — the listener loop
                    │       └── handleConn(rootCtx, ...)    — one per accepted conn
                    ├── runStatsDisplay(rootCtx, ...)        — ticker printer
                    └── runCLI(rootCtx, cancel, ...)         — stdin reader
```

### Why carry `*Config` in context instead of a global?

- The server package has no `init()` or package-level state.
- `server.Run` is a pure function: given a context and a stats pointer, it
  runs a server. This makes it trivially testable — pass a test context with
  a test config, no globals to reset.
- If you later want to run two servers in the same process (e.g. for testing),
  you just call `Run` twice with two different contexts.

---

## 2. Goroutine Inventory

| Goroutine           | Created by        | Ends when                                    |
|---------------------|-------------------|----------------------------------------------|
| `server.Run`        | `main`            | `ctx.Done()` → listener closed               |
| `handleConn`        | `server.Run`      | client disconnects, idle timeout, or `ctx.Done()` |
| `runStatsDisplay`   | `main`            | `ctx.Done()`                                 |
| `runCLI`            | `main`            | `ctx.Done()` or user types `q`               |
| listener-closer     | `server.Run`      | `ctx.Done()` (one-shot, exits after close)   |

No goroutine leaks: every goroutine either has a `<-ctx.Done()` exit arm or
does finite work and returns.

---

## 3. The Listener Shutdown Trick

`net.Listener.Accept()` blocks forever. There is no way to pass a context to
it directly. The standard Go pattern to make it cancellable is:

```go
// In server.Run — runs once, one-shot goroutine
go func() {
    <-ctx.Done()
    ln.Close() // this makes Accept() return an error immediately
}()

for {
    conn, err := ln.Accept()
    if err != nil {
        select {
        case <-ctx.Done():
            return nil // expected — clean shutdown
        default:
            log.Printf("unexpected accept error: %v", err)
            continue
        }
    }
    go handleConn(...)
}
```

The `select` after the error is essential: `ln.Close()` always causes
`Accept()` to return an error. Without the `ctx.Done()` check, the loop
would log a spurious error on every clean shutdown.

---

## 4. Per-Connection Goroutines

Every accepted connection gets its own goroutine (`handleConn`). This is the
standard Go TCP server pattern — simple, and the scheduler handles thousands
of goroutines efficiently.

### Read deadline

```go
conn.SetReadDeadline(time.Now().Add(30 * time.Second))
```

Without a read deadline, a client that connects and then goes silent (e.g.
`idleClient`) holds a goroutine open forever. The 30-second timeout closes
idle connections and prevents goroutine accumulation.

If you want to allow truly persistent connections (e.g. for `streamClient`),
reset the deadline after every successful read:

```go
// inside the read loop, after a successful ReadString:
conn.SetReadDeadline(time.Now().Add(30 * time.Second))
```

This is already the behaviour in `handleConn` — each iteration of the loop
sets a fresh deadline, so an active connection never times out, but an idle
one does after 30s of silence.

---

## 5. The Controlled Delay

```go
// applyDelay — in server.go
func applyDelay(ctx context.Context, cfg *Config) bool {
    delayMs := cfg.Delay() // atomic read — always current value
    if delayMs <= 0 {
        return true
    }
    select {
    case <-ctx.Done():
        return false           // shutdown during delay — close connection
    case <-time.After(...):
        return true            // delay elapsed normally
    }
}
```

### Why atomic for delay, not a mutex?

`cfg.delayMs` is a single `int64`. The CLI goroutine writes it with
`SetDelay`; every `handleConn` goroutine reads it with `Delay()`. An
`atomic.Int64` is the right tool — no lock-ordering risk, cheaper than a
mutex, and the Go memory model guarantees visibility across goroutines.

### Why re-read delay per request, not per connection?

Reading `cfg.Delay()` inside `applyDelay` (called per request) means a
`d 500` command takes effect on the very next request — even on a connection
that has been open for minutes. If you read it once at connection open time
you'd have to reconnect to see changes.

---

## 6. Stats Counters

All fields in `Stats` are `atomic.Int64`:

```go
stats.ActiveConns.Add(1)   // connection opened
stats.ActiveConns.Add(-1)  // deferred in handleConn — always fires on exit
stats.TotalRequests.Add(1) // inside dispatch, on REQ
```

The stats display goroutine reads these from a different goroutine with no
locking — that is safe because atomic reads are always consistent.

Note: `ActiveConns` can briefly read as negative if a connection closes
between the `Add(-1)` and the next display tick. This is cosmetic — over time
it self-corrects.

---

## 7. Protocol Reference

Line-delimited text, `\n` terminated. Matches `pkgs/clients`.

| Client sends    | Server responds      | Delay applied? | Stat incremented   |
|-----------------|----------------------|----------------|--------------------|
| `REQ\n`         | `ACK\n`              | yes            | `TotalRequests`    |
| `REQ <seq>\n`   | `ACK\n`              | yes            | `TotalRequests`    |
| `PING <id>\n`   | `PONG <id>\n`        | yes            | `TotalPings`       |
| `STREAM ...\n`  | `STREAM_ACK\n`       | **no**         | —                  |
| `SLOW <id>\n`   | `ACK\n`              | yes            | `TotalRequests`    |
| anything else   | `ERR unknown ...\n`  | no             | —                  |

Stream messages are not delayed because `streamClient` sends at 200ms
intervals. Adding a server delay larger than 200ms would cause writes to pile
up faster than the client drains them, eventually filling the TCP send buffer
and blocking the client's `Write`. This is realistic behaviour — but it is
better to test it deliberately rather than accidentally.

---

## 8. Shutdown Sequence

```
user types "q"
    → runCLI calls cancel()
        → rootCtx.Done() closes
            → listener-closer goroutine calls ln.Close()
                → Accept() returns error → server.Run returns nil
            → every handleConn sees ctx.Done() at next select, returns
            → runStatsDisplay sees ctx.Done(), returns
            → runCLI returns
    → main unblocks from serverDone channel
    → printStats prints final snapshot
    → process exits
```

`main` also has a 3-second timeout waiting for `server.Run` to return, in
case a `handleConn` goroutine is mid-delay and takes a moment to notice
cancellation. In practice `applyDelay` responds to `ctx.Done()` immediately,
so the timeout is just a safety net.

---

## 9. Simulating Horizontal Scaling

Run one process per terminal:

```bash
# Terminal 1
go run ./cmd/server
# port: 8080, id: server-1, delay: 0

# Terminal 2
go run ./cmd/server
# port: 8081, id: server-2, delay: 100

# Terminal 3
go run ./cmd/server
# port: 8082, id: server-3, delay: 500
```

Each process is completely isolated — no shared memory, no shared file
descriptors. This accurately models real horizontal scaling. The load balancer
(next component) connects to all three ports and decides how to distribute
connections across them.

Varying the delay per instance lets you observe how the LB handles backends
with different response speeds — does it keep sending to a slow backend, or
does it route around it?

---

## 10. Future Work / TODOs

- [ ] Add `chaos` command: `c <pct>` randomly closes `pct`% of incoming
      connections immediately after accept — simulates a flaky backend.
- [ ] Add `hang` command: `h <ms>` makes the server accept but never respond
      for `ms` ms — simulates a server that is alive but overloaded.
- [ ] Expose stats via a lightweight HTTP endpoint (e.g. `:9090/stats`) so
      the LB can scrape health metrics without a TCP connection.
- [ ] Add `maxConns` to `Config` — refuse connections above a threshold to
      simulate capacity limits.
- [ ] Structured logging (e.g. `slog`) so log lines can be parsed by a
      dashboard.
