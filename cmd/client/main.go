// cmd/client/main.go
//
// Client Simulation CLI
// ─────────────────────
// Two spawners run in parallel:
//
//  1. Constant spawner  — quietly launches N clients every second automatically.
//  2. Burst spawner     — waits for your keyboard input to fire large batches.
//
// Commands (type into stdin while running):
//
//	<number>   multiply by 100 and burst that many clients immediately
//	           e.g.  "5"  →  500 clients right now
//	r<number>  change the constant-spawner rate (clients per second)
//	           e.g.  "r3" →  3 clients/sec from now on
//	q          cancel all clients and exit cleanly
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Asthraris/L4-proxy-LB/pkgs/clients"
)

func main() {
	printBanner()

	url := promptURL()

	// ── Root context ──────────────────────────────────────────────────────────
	// Everything — spawners, every client goroutine — shares this context.
	// Calling cancel() (via "q") is the single shutdown signal for the whole
	// program.  See notes/CONCURRENCY.md for a full explanation.
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Inject the URL so every package function can reach it via ctx without
	// needing it passed as an extra parameter through every call stack frame.
	rootCtx = context.WithValue(rootCtx, clients.URL_KEY, url)

	// ── Constant spawner rate (atomic so both goroutines can touch it safely) ─
	var constantRate atomic.Int64
	constantRate.Store(2) // default: 2 clients/sec at startup

	// ── Constant spawner ──────────────────────────────────────────────────────
	// Runs until ctx is cancelled.  Rate is re-read every tick so "r<n>"
	// changes take effect on the very next second.
	go runConstantSpawner(rootCtx, &constantRate)

	// ── Burst spawner (stdin reader) ──────────────────────────────────────────
	go runBurstSpawner(rootCtx, cancel, &constantRate)

	// Block main until the context is cancelled (either "q" or Ctrl-C).
	<-rootCtx.Done()
	fmt.Println("\n[main] context cancelled — waiting for spawners to stop...")

	// Give goroutines a moment to log their shutdown messages before os.Exit.
	time.Sleep(200 * time.Millisecond)
	fmt.Println("[main] simulation ended. goodbye.")
}

// ─────────────────────────────────────────────────────────────────────────────
// runConstantSpawner ticks every second and calls LaunchClients with whatever
// the current rate atomic holds.  Changing the atomic from the burst goroutine
// is safe — no mutex needed for a single int64.
// ─────────────────────────────────────────────────────────────────────────────
func runConstantSpawner(ctx context.Context, rate *atomic.Int64) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	log.Printf("[constant] spawner started at %d clients/sec", rate.Load())

	for {
		select {
		case <-ctx.Done():
			log.Println("[constant] spawner stopped")
			return

		case <-ticker.C:
			n := int(rate.Load())
			if n <= 0 {
				continue
			}
			clients.LaunchClients(ctx, n)
			log.Printf("[constant] spawned %d clients", n)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// runBurstSpawner blocks on stdin.  It is the interactive control plane.
//
// The goroutine owns the reader; main never touches stdin directly after this
// is launched, so there is no data race on os.Stdin.
// ─────────────────────────────────────────────────────────────────────────────
func runBurstSpawner(ctx context.Context, cancel context.CancelFunc, rate *atomic.Int64) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("[burst]  ready — commands: <number>  r<number>  q")

	for {
		// Check context before blocking on stdin
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			log.Printf("[burst] stdin read error: %v", err)
			continue
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		switch {
		case line == "q":
			fmt.Println("[burst]  shutting down...")
			cancel()
			return

		case strings.HasPrefix(line, "r"):
			// Change constant-spawner rate: "r5" → 5 clients/sec
			rawN := strings.TrimPrefix(line, "r")
			n, err := strconv.Atoi(rawN)
			if err != nil || n < 0 {
				fmt.Println("[burst]  usage: r<number>  e.g. r5 sets 5 clients/sec")
				continue
			}
			rate.Store(int64(n))
			fmt.Printf("[burst]  constant rate changed to %d clients/sec\n", n)

		default:
			// Plain number → burst (multiply by 100)
			n, err := strconv.Atoi(line)
			if err != nil || n <= 0 {
				fmt.Println("[burst]  enter a number, r<number>, or q")
				continue
			}
			total := n * 100
			fmt.Printf("[burst]  firing %d clients  [%s]\n",
				total, time.Now().Format("04:05"))
			go clients.LaunchClients(ctx, total)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

func promptURL() string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("target (host:port) : ")
	line, err := reader.ReadString('\n')
	if err != nil {
		log.Fatal("could not read URL:", err)
	}
	url := strings.TrimSpace(line)
	if url == "" {
		log.Fatal("URL cannot be empty")
	}
	return url
}

func printBanner() {
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println("               Client Simulation — L4 Proxy Load Test            ")
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println("  Spawners:")
	fmt.Println("    constant  —  N clients/sec, running automatically")
	fmt.Println("    burst     —  fire a big batch on demand via stdin")
	fmt.Println()
	fmt.Println("  Commands:")
	fmt.Println("    <number>    burst  number×100  clients right now")
	fmt.Println("    r<number>   set constant-spawner rate (clients/sec)")
	fmt.Println("    q           quit cleanly")
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println()
}
