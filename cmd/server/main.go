// cmd/server/main.go
//
// Server Instance CLI
// ───────────────────
// Starts a single TCP server instance.  Run this once per terminal to
// simulate horizontal scaling — each process is fully isolated.
//
// Startup prompts:
//   port      — TCP port to listen on  (e.g. 8080)
//   delay     — initial response delay in ms  (0 = no delay)
//   server id — human label shown in logs     (e.g. "server-1")
//
// Commands (type while running):
//
//	d <ms>    set response delay in milliseconds  e.g. "d 200"
//	d 0       remove delay entirely
//	s         print current stats snapshot
//	q         graceful shutdown
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Asthraris/L4-proxy-LB/pkgs/server"
)

func main() {
	printBanner()

	// ── Startup config ────────────────────────────────────────────────────────
	cfg, stats := promptConfig()

	// ── Root context ──────────────────────────────────────────────────────────
	// ctx carries the *Config into the server package.
	// cancel() is the single shutdown signal — it closes the listener,
	// which unblocks Accept(), and every handleConn goroutine checks ctx.Done()
	// between reads.  See notes/SERVER.md §Shutdown for the full sequence.
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rootCtx = context.WithValue(rootCtx, server.ConfigKey, cfg)

	// ── Server goroutine ──────────────────────────────────────────────────────
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(rootCtx, stats)
	}()

	// ── Live stats display ────────────────────────────────────────────────────
	go runStatsDisplay(rootCtx, cfg, stats)

	// ── CLI command loop ──────────────────────────────────────────────────────
	go runCLI(rootCtx, cancel, cfg, stats)

	// Block until either the server errors out or the CLI cancels the context.
	select {
	case err := <-serverDone:
		if err != nil {
			log.Printf("[main] server exited with error: %v", err)
		}
	case <-rootCtx.Done():
		// CLI typed "q" — wait for server to finish its cleanup
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			log.Println("[main] server did not stop within 3s — forcing exit")
		}
	}

	fmt.Printf("\n[%s] final stats:\n", cfg.ID)
	printStats(cfg, stats)
	fmt.Println("[main] goodbye.")
}

// ─────────────────────────────────────────────────────────────────────────────
// runCLI reads commands from stdin and applies them.
//
// Commands:
//
//	d <ms>  — set delay
//	s       — print stats now
//	q       — shutdown
// ─────────────────────────────────────────────────────────────────────────────
func runCLI(ctx context.Context, cancel context.CancelFunc, cfg *server.Config, stats *server.Stats) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("[cli]  ready — commands: d <ms>   s   q")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			log.Printf("[cli] stdin error: %v", err)
			continue
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)

		switch parts[0] {

		case "q":
			fmt.Println("[cli]  shutting down...")
			cancel()
			return

		case "s":
			printStats(cfg, stats)

		case "d":
			if len(parts) < 2 {
				fmt.Println("[cli]  usage: d <milliseconds>  e.g.  d 300")
				continue
			}
			ms, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || ms < 0 {
				fmt.Println("[cli]  delay must be a non-negative integer (ms)")
				continue
			}
			cfg.SetDelay(ms)
			fmt.Printf("[cli]  delay set to %dms\n", ms)

		default:
			fmt.Printf("[cli]  unknown command %q — try: d <ms>  s  q\n", parts[0])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runStatsDisplay prints a one-line stats summary every 2 seconds.
// Stops cleanly when ctx is cancelled.
// ─────────────────────────────────────────────────────────────────────────────
func runStatsDisplay(ctx context.Context, cfg *server.Config, stats *server.Stats) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			printStats(cfg, stats)
		}
	}
}

// printStats prints a single formatted stats line to stdout.
func printStats(cfg *server.Config, stats *server.Stats) {
	fmt.Printf(
		"[%s]  port=%-6s  active=%-4d  total_conns=%-6d  reqs=%-6d  pings=%-6d  delay=%dms\n",
		cfg.ID,
		cfg.Port,
		stats.ActiveConns.Load(),
		stats.TotalConns.Load(),
		stats.TotalRequests.Load(),
		stats.TotalPings.Load(),
		cfg.Delay(),
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// promptConfig gathers startup configuration from the user interactively.
// ─────────────────────────────────────────────────────────────────────────────
func promptConfig() (*server.Config, *server.Stats) {
	reader := bufio.NewReader(os.Stdin)

	port := prompt(reader, "port (e.g. 8080)        : ")
	idStr := prompt(reader, "server id (e.g. server-1): ")
	delayStr := prompt(reader, "initial delay ms (0=none): ")

	delayMs, err := strconv.ParseInt(delayStr, 10, 64)
	if err != nil || delayMs < 0 {
		delayMs = 0
	}

	cfg := &server.Config{
		Port: port,
		ID:   idStr,
	}
	cfg.SetDelay(delayMs)

	return cfg, &server.Stats{}
}

func prompt(r *bufio.Reader, label string) string {
	fmt.Print(label)
	line, err := r.ReadString('\n')
	if err != nil {
		log.Fatal("prompt read error:", err)
	}
	return strings.TrimSpace(line)
}

// ─────────────────────────────────────────────────────────────────────────────
// printBanner prints the startup header.
// ─────────────────────────────────────────────────────────────────────────────
func printBanner() {
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println("                  TCP Server Instance — L4 Proxy                 ")
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println("  Each instance is fully isolated — run one per terminal to")
	fmt.Println("  simulate horizontal scaling.")
	fmt.Println()
	fmt.Println("  Commands (after startup):")
	fmt.Println("    d <ms>    set response delay in milliseconds")
	fmt.Println("    s         print current stats")
	fmt.Println("    q         graceful shutdown")
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println()
}
