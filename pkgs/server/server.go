package server

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Run — the server's entry point.
//
// Blocks until ctx is cancelled.  Call it in its own goroutine from main,
// or just call it directly and let main block on it — either is fine.
//
// Requires a *Config to be present in ctx under ConfigKey.
// ─────────────────────────────────────────────────────────────────────────────

func Run(ctx context.Context, stats *Stats) error {
	cfg, ok := configFromCtx(ctx)
	if !ok {
		return fmt.Errorf("server.Run: *Config not found in context — set server.ConfigKey")
	}

	ln, err := net.Listen("tcp", ":"+cfg.Port)
	if err != nil {
		return fmt.Errorf("[%s] listen on :%s — %w", cfg.ID, cfg.Port, err)
	}

	log.Printf("[%s] listening on :%s  (delay=%dms)", cfg.ID, cfg.Port, cfg.Delay())

	// Close the listener when ctx is cancelled — this unblocks Accept().
	go func() {
		<-ctx.Done()
		ln.Close()
		log.Printf("[%s] listener closed", cfg.ID)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// A closed listener returns an error — check if it was intentional.
			select {
			case <-ctx.Done():
				return nil // clean shutdown
			default:
				log.Printf("[%s] accept error: %v", cfg.ID, err)
				continue
			}
		}

		stats.ActiveConns.Add(1)
		stats.TotalConns.Add(1)

		go handleConn(ctx, conn, cfg, stats)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleConn — runs in its own goroutine, one per accepted connection.
//
// Protocol (line-delimited text, matching pkgs/clients):
//
//	REQ\n      → delay, then respond ACK\n
//	PING <id>\n → delay, then respond PONG <id>\n
//	STREAM ...\n → acknowledge with STREAM_ACK\n  (no delay — stream is high-freq)
//	anything else → respond ERR unknown\n
// ─────────────────────────────────────────────────────────────────────────────

func handleConn(ctx context.Context, conn net.Conn, cfg *Config, stats *Stats) {
	defer func() {
		conn.Close()
		stats.ActiveConns.Add(-1)
	}()

	remoteAddr := conn.RemoteAddr().String()
	log.Printf("[%s] connection opened  from %s", cfg.ID, remoteAddr)

	reader := bufio.NewReader(conn)

	for {
		// Respect cancellation between reads
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Set a read deadline so we don't block forever on idle connections.
		// If the client goes idle the read will timeout and we clean up.
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		line, err := reader.ReadString('\n')
		if err != nil {
			// Timeout or closed connection — either is a normal exit path
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("[%s] idle timeout  %s", cfg.ID, remoteAddr)
			}
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		response := dispatch(ctx, line, cfg, stats)
		if response == "" {
			// dispatch returned "" meaning we should close (e.g. ctx cancelled mid-delay)
			return
		}

		if _, err := fmt.Fprint(conn, response); err != nil {
			log.Printf("[%s] write error  %s: %v", cfg.ID, remoteAddr, err)
			return
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// dispatch decides what to do with a single received line and returns the
// response string (including trailing \n).
//
// Returns "" if the connection should be closed (ctx cancelled during delay).
// ─────────────────────────────────────────────────────────────────────────────

func dispatch(ctx context.Context, line string, cfg *Config, stats *Stats) string {
	switch {
	case line == "REQ" || strings.HasPrefix(line, "REQ "):
		stats.TotalRequests.Add(1)
		if !applyDelay(ctx, cfg) {
			return ""
		}
		return "ACK\n"

	case strings.HasPrefix(line, "PING"):
		stats.TotalPings.Add(1)
		// Extract the id after PING if present: "PING 42" → "PONG 42"
		id := strings.TrimPrefix(line, "PING")
		id = strings.TrimSpace(id)
		if !applyDelay(ctx, cfg) {
			return ""
		}
		if id != "" {
			return fmt.Sprintf("PONG %s\n", id)
		}
		return "PONG\n"

	case strings.HasPrefix(line, "STREAM"):
		// Stream clients send at high frequency — we don't delay stream acks
		// because that would cause unbounded buffering on the client side.
		return "STREAM_ACK\n"

	case strings.HasPrefix(line, "SLOW"):
		// slowClient sends byte-by-byte; once we have the full line, treat like REQ.
		stats.TotalRequests.Add(1)
		if !applyDelay(ctx, cfg) {
			return ""
		}
		return "ACK\n"

	default:
		return fmt.Sprintf("ERR unknown command: %q\n", line)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// applyDelay sleeps for the configured delay, but wakes immediately if ctx
// is cancelled.  Returns false if cancelled during the sleep.
// ─────────────────────────────────────────────────────────────────────────────

func applyDelay(ctx context.Context, cfg *Config) bool {
	delayMs := cfg.Delay()
	if delayMs <= 0 {
		return true
	}

	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(delayMs) * time.Millisecond):
		return true
	}
}
