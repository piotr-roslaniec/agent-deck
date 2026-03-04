package mcppool

import (
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
