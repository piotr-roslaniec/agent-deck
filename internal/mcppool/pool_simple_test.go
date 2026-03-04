package mcppool

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestartsWithinWindow_BoundaryTimestampIsCounted(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 1, 59, 0, time.UTC)
	history := []time.Time{
		now.Add(-restartWindowDuration),                   // exact boundary
		now.Add(-59 * time.Second),                        // inside window
		now.Add(-30 * time.Second),                        // inside window
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

func TestResetPermanentlyFailedProxy_NotFound(t *testing.T) {
	pool := &Pool{proxies: map[string]*SocketProxy{}}

	err := pool.ResetPermanentlyFailedProxy("missing")
	if err == nil {
		t.Fatal("expected error for missing proxy")
	}
	if !strings.Contains(err.Error(), "proxy missing not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResetPermanentlyFailedProxy_NotPermanentlyFailed(t *testing.T) {
	pool := &Pool{
		proxies: map[string]*SocketProxy{
			"test": {Status: StatusFailed},
		},
	}

	err := pool.ResetPermanentlyFailedProxy("test")
	if err == nil {
		t.Fatal("expected error for non-permanently failed proxy")
	}
	if !strings.Contains(err.Error(), "is not permanently failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResetPermanentlyFailedProxy_RestartFailureIsReturned(t *testing.T) {
	name := "broken"
	pool := &Pool{
		proxies: map[string]*SocketProxy{
			name: {
				name:          name,
				command:       "/definitely/missing/command",
				args:          nil,
				env:           map[string]string{},
				clients:       make(map[string]net.Conn),
				requestMap:    make(map[interface{}]string),
				Status:        StatusPermanentlyFailed,
				totalFailures: maxTotalRestartFailures,
				restartCount:  4,
				lastRestart:   time.Now(),
				restartWindow: []time.Time{time.Now()},
			},
		},
		ctx: context.Background(),
	}

	err := pool.ResetPermanentlyFailedProxy(name)
	if err == nil {
		t.Fatal("expected reset restart failure")
	}
	if !strings.Contains(err.Error(), "failed to restart proxy broken after reset") {
		t.Fatalf("unexpected error: %v", err)
	}

	proxy := pool.proxies[name]
	if proxy == nil {
		t.Fatal("expected proxy to remain tracked after failed restart")
	}
	if got := proxy.GetStatus(); got != StatusFailed {
		t.Fatalf("expected failed status after restart failure, got %s", got)
	}
	if proxy.totalFailures != 1 {
		t.Fatalf("expected total failures to restart from fresh counter, got %d", proxy.totalFailures)
	}
	if proxy.restartCount != 1 {
		t.Fatalf("expected restart count to restart from fresh counter, got %d", proxy.restartCount)
	}
}

func TestResetPermanentlyFailedProxy_Success(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := NewPool(ctx, &PoolConfig{Enabled: true})
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.Shutdown()
	})

	name := fmt.Sprintf("reset-success-%d", time.Now().UnixNano())
	if err := pool.Start(name, "cat", nil, nil); err != nil {
		t.Fatalf("failed to start proxy for test: %v", err)
	}

	pool.mu.Lock()
	proxy := pool.proxies[name]
	proxy.SetStatus(StatusPermanentlyFailed)
	proxy.totalFailures = maxTotalRestartFailures
	proxy.restartCount = 4
	proxy.lastRestart = time.Now()
	proxy.restartWindow = []time.Time{
		time.Now().Add(-3 * time.Second),
		time.Now().Add(-2 * time.Second),
	}
	pool.mu.Unlock()

	if err := pool.ResetPermanentlyFailedProxy(name); err != nil {
		t.Fatalf("expected reset to succeed, got: %v", err)
	}

	pool.mu.RLock()
	resetProxy := pool.proxies[name]
	status := resetProxy.GetStatus()
	totalFailures := resetProxy.totalFailures
	restartCount := resetProxy.restartCount
	lastRestart := resetProxy.lastRestart
	restartWindowLen := len(resetProxy.restartWindow)
	pool.mu.RUnlock()

	if status != StatusRunning {
		t.Fatalf("expected running status after successful reset, got %s", status)
	}
	if totalFailures != 0 {
		t.Fatalf("expected total failures reset to 0, got %d", totalFailures)
	}
	if restartCount != 1 {
		t.Fatalf("expected fresh restart history count of 1, got %d", restartCount)
	}
	if lastRestart.IsZero() {
		t.Fatal("expected lastRestart to be set by fresh attempt")
	}
	if restartWindowLen != 1 {
		t.Fatalf("expected fresh restart window to contain only new attempt, got %d entries", restartWindowLen)
	}
}

func TestDiscoverExistingSocketsWith_RemovesStaleSocketFile(t *testing.T) {
	tmpDir := t.TempDir()
	staleSocketPath := filepath.Join(tmpDir, "agentdeck-mcp-stale.sock")
	if err := os.WriteFile(staleSocketPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("failed to create stale socket placeholder: %v", err)
	}

	pool := &Pool{proxies: map[string]*SocketProxy{}}
	discovered := pool.discoverExistingSocketsWith("ignored", socketDiscoveryDeps{
		glob: func(pattern string) ([]string, error) {
			return []string{staleSocketPath}, nil
		},
		isAlive: func(socketPath string) bool {
			return false
		},
		register: func(name, socketPath string) error {
			t.Fatalf("register should not be called for stale socket %s", socketPath)
			return nil
		},
	})

	if discovered != 0 {
		t.Fatalf("expected no discovered sockets, got %d", discovered)
	}
	if _, err := os.Stat(staleSocketPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale socket file to be removed, stat err=%v", err)
	}
}

func TestDiscoverExistingSocketsWith_DiscoversLiveAndSkipsExisting(t *testing.T) {
	tmpDir := t.TempDir()
	staleSocketPath := filepath.Join(tmpDir, "agentdeck-mcp-stale.sock")
	existingSocketPath := filepath.Join(tmpDir, "agentdeck-mcp-existing.sock")
	liveSocketPath := filepath.Join(tmpDir, "agentdeck-mcp-live.sock")
	invalidSocketPath := filepath.Join(tmpDir, "random.sock")

	for _, path := range []string{staleSocketPath, existingSocketPath, liveSocketPath} {
		if err := os.WriteFile(path, []byte("sock"), 0o600); err != nil {
			t.Fatalf("failed to create socket placeholder %s: %v", path, err)
		}
	}

	pool := &Pool{
		proxies: map[string]*SocketProxy{
			"existing": {},
		},
	}

	var removedPaths []string
	type registerCall struct {
		name string
		path string
	}
	var registerCalls []registerCall

	discovered := pool.discoverExistingSocketsWith("custom-pattern", socketDiscoveryDeps{
		glob: func(pattern string) ([]string, error) {
			if pattern != "custom-pattern" {
				t.Fatalf("unexpected pattern %q", pattern)
			}
			return []string{invalidSocketPath, staleSocketPath, existingSocketPath, liveSocketPath}, nil
		},
		isAlive: func(socketPath string) bool {
			return socketPath == existingSocketPath || socketPath == liveSocketPath
		},
		remove: func(path string) error {
			removedPaths = append(removedPaths, path)
			return nil
		},
		register: func(name, socketPath string) error {
			registerCalls = append(registerCalls, registerCall{name: name, path: socketPath})
			return nil
		},
	})

	if discovered != 1 {
		t.Fatalf("expected exactly one discovered socket, got %d", discovered)
	}
	if len(removedPaths) != 1 || removedPaths[0] != staleSocketPath {
		t.Fatalf("expected only stale socket removal, got %v", removedPaths)
	}
	if len(registerCalls) != 1 {
		t.Fatalf("expected one live socket registration, got %d", len(registerCalls))
	}
	if registerCalls[0].name != "live" || registerCalls[0].path != liveSocketPath {
		t.Fatalf("expected live socket registration, got %+v", registerCalls[0])
	}
	if _, err := os.Stat(liveSocketPath); err != nil {
		t.Fatalf("expected live socket placeholder to be preserved, stat err=%v", err)
	}
}
