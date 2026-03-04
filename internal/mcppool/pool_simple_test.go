package mcppool

import (
	"strings"
	"testing"
	"time"
)

func TestRestartsWithinWindow_BoundaryTimestampIsCounted(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 1, 59, 0, time.UTC)
	history := []time.Time{
		now.Add(-restartWindowDuration),      // exact boundary
		now.Add(-59 * time.Second),           // inside window
		now.Add(-30 * time.Second),           // inside window
		now.Add(-restartWindowDuration - time.Nanosecond), // outside window
	}

	recent := restartsWithinWindow(history, now)
	if len(recent) != 3 {
		t.Fatalf("expected 3 restarts in window, got %d", len(recent))
	}
	if len(recent) < maxRestartsPerMinute {
		t.Fatalf("expected limiter to block at boundary, got %d restarts", len(recent))
	}
}

func TestRestartsWithinWindow_ExpiredTimestampIsPruned(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 1, 59, 0, time.UTC)
	history := []time.Time{
		now.Add(-restartWindowDuration - time.Nanosecond), // just outside window
		now.Add(-59 * time.Second),                        // inside window
		now.Add(-10 * time.Second),                        // inside window
	}

	recent := restartsWithinWindow(history, now)
	if len(recent) != 2 {
		t.Fatalf("expected 2 restarts in window, got %d", len(recent))
	}
	if len(recent) >= maxRestartsPerMinute {
		t.Fatalf("expected limiter to allow restart, got %d restarts", len(recent))
	}
}

func TestRestartProxyWithRateLimit_PermanentlyDisablesAtFiveFailures(t *testing.T) {
	const failuresAtLimit = 5
	proxy := &SocketProxy{
		Status:        StatusFailed,
		totalFailures: failuresAtLimit,
		lastRestart:   time.Now(),
	}
	pool := &Pool{
		proxies: map[string]*SocketProxy{
			"test": proxy,
		},
	}

	err := pool.RestartProxyWithRateLimit("test")
	if err == nil {
		t.Fatal("expected permanent disable error, got nil")
	}
	if !strings.Contains(err.Error(), "permanently disabled after 5 failures") {
		t.Fatalf("expected permanent disable at 5 failures, got %q", err.Error())
	}
	if got := proxy.GetStatus(); got != StatusPermanentlyFailed {
		t.Fatalf("expected status %s, got %s", StatusPermanentlyFailed, got)
	}
}

func TestRestartProxyWithRateLimit_FourFailuresNotPermanentlyDisabled(t *testing.T) {
	const failuresBelowLimit = 4
	proxy := &SocketProxy{
		Status:        StatusFailed,
		totalFailures: failuresBelowLimit,
		lastRestart:   time.Now(),
	}
	pool := &Pool{
		proxies: map[string]*SocketProxy{
			"test": proxy,
		},
	}

	err := pool.RestartProxyWithRateLimit("test")
	if err == nil {
		t.Fatal("expected rate-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "rate limited:") {
		t.Fatalf("expected rate-limit error below failure cap, got %q", err.Error())
	}
	if got := proxy.GetStatus(); got == StatusPermanentlyFailed {
		t.Fatalf("expected proxy not to be permanently failed below cap, got %s", got)
	}
}
