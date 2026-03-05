package server

import (
	"context"
	"sync/atomic"
)

// ─────────────────────────────────────────────────────────────────────────────
// Context keys
// ─────────────────────────────────────────────────────────────────────────────

// contextKey is an unexported named type for context keys in this package.
// Prevents collisions with keys from any other package.
type contextKey string

const (
	// ConfigKey is the context key that carries a *Config into the server.
	// Set by cmd/server/main.go before starting the server.
	//
	//   ctx = context.WithValue(ctx, server.ConfigKey, &server.Config{...})
	ConfigKey contextKey = "config"
)

// ─────────────────────────────────────────────────────────────────────────────
// Config — immutable at startup, delay is live-adjustable via atomic
// ─────────────────────────────────────────────────────────────────────────────

// Config holds everything a server instance needs to start.
// Port and ID are fixed at launch; DelayMs can be changed at runtime
// by the CLI without restarting — it is stored as an atomic int64.
type Config struct {
	// Port is the TCP port this server listens on. e.g. "8080"
	Port string

	// ID is a human-readable label shown in logs. e.g. "server-1"
	ID string

	// delayMs is the per-request artificial delay in milliseconds.
	// Use Config.SetDelay / Config.Delay to access it safely.
	delayMs atomic.Int64
}

// SetDelay atomically updates the response delay.
// Safe to call from the CLI goroutine while the server is running.
func (c *Config) SetDelay(ms int64) {
	c.delayMs.Store(ms)
}

// Delay returns the current response delay in milliseconds.
func (c *Config) Delay() int64 {
	return c.delayMs.Load()
}

// ─────────────────────────────────────────────────────────────────────────────
// Stats — live counters, all updated atomically (no mutex needed)
// ─────────────────────────────────────────────────────────────────────────────

// Stats tracks live server metrics.
// Every field is an atomic so the CLI display goroutine can read them
// concurrently with connection handler goroutines writing them.
type Stats struct {
	// ActiveConns is the number of connections currently open.
	ActiveConns atomic.Int64

	// TotalConns is the total number of connections accepted since start.
	TotalConns atomic.Int64

	// TotalRequests is the total number of REQ messages processed.
	TotalRequests atomic.Int64

	// TotalPings is the total number of PING messages processed.
	TotalPings atomic.Int64

	// TotalDropped is the number of connections dropped in chaos mode.
	// Reserved for future use — wired up but chaos is not enabled by default.
	TotalDropped atomic.Int64
}

// configFromCtx extracts the *Config from a context.
// Returns nil, false if the value is missing or wrong type.
func configFromCtx(ctx context.Context) (*Config, bool) {
	v := ctx.Value(ConfigKey)
	cfg, ok := v.(*Config)
	return cfg, ok
}
