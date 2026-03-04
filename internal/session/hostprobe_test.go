package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeHostProber struct {
	calls int
}

func (f *fakeHostProber) Probe(ctx context.Context, sshHost string) (HostMetrics, error) {
	_ = ctx
	_ = sshHost
	f.calls++
	return HostMetrics{CPUPercent: 10, RAMPercent: 20, DiskPercent: 30}, nil
}

func setHostProbeSettingsForTest(t *testing.T, threshold float64, ttl time.Duration) {
	t.Helper()
	prevThreshold := remoteOverloadThresholdProvider
	prevTTL := remoteProbeCacheTTLProvider

	remoteOverloadThresholdProvider = func() float64 { return threshold }
	remoteProbeCacheTTLProvider = func() time.Duration { return ttl }

	t.Cleanup(func() {
		remoteOverloadThresholdProvider = prevThreshold
		remoteProbeCacheTTLProvider = prevTTL
	})
}

func TestParseProbeOutput_Valid(t *testing.T) {
	output := "1.5 4\n45\n32\n"
	m, err := ParseProbeOutput(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.CPUPercent != 37.5 { // 1.5/4 * 100
		t.Errorf("CPUPercent = %v, want 37.5", m.CPUPercent)
	}
	if m.RAMPercent != 45 {
		t.Errorf("RAMPercent = %v, want 45", m.RAMPercent)
	}
	if m.DiskPercent != 32 {
		t.Errorf("DiskPercent = %v, want 32", m.DiskPercent)
	}
}

func TestParseProbeOutput_Malformed(t *testing.T) {
	_, err := ParseProbeOutput("invalid")
	if err == nil {
		t.Error("expected error for malformed input")
	}
}

func TestParseProbeOutput_Empty(t *testing.T) {
	_, err := ParseProbeOutput("")
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestScoring_MinPeak(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	results := []HostProbeResult{
		{Name: "high", Metrics: HostMetrics{CPUPercent: 70, RAMPercent: 30, DiskPercent: 20}},
		{Name: "low", Metrics: HostMetrics{CPUPercent: 20, RAMPercent: 20, DiskPercent: 20}},
	}
	best, err := SelectBestHost(results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if best.Name != "low" {
		t.Errorf("best = %s, want low", best.Name)
	}
}

func TestScoring_Tiebreak(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	results := []HostProbeResult{
		{Name: "higher-avg", Metrics: HostMetrics{CPUPercent: 50, RAMPercent: 50, DiskPercent: 30}},
		{Name: "lower-avg", Metrics: HostMetrics{CPUPercent: 50, RAMPercent: 20, DiskPercent: 10}},
	}
	best, err := SelectBestHost(results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if best.Name != "lower-avg" {
		t.Errorf("best = %s, want lower-avg", best.Name)
	}
}

func TestScoring_AllOverloaded(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	results := []HostProbeResult{
		{Name: "a", Metrics: HostMetrics{CPUPercent: 90, RAMPercent: 90, DiskPercent: 90}},
	}
	_, err := SelectBestHost(results)
	if err == nil {
		t.Error("expected error for all overloaded")
	}
	if !strings.Contains(err.Error(), "threshold: 80%") {
		t.Fatalf("expected overload error to include threshold, got: %v", err)
	}
}

func TestScoring_SingleHost(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	results := []HostProbeResult{
		{Name: "only", Metrics: HostMetrics{CPUPercent: 30, RAMPercent: 40, DiskPercent: 50}},
	}
	best, err := SelectBestHost(results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if best.Name != "only" {
		t.Errorf("best = %s, want only", best.Name)
	}
}

func TestScoring_UnreachableSkipped(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	results := []HostProbeResult{
		{Name: "dead", Err: fmt.Errorf("timeout")},
		{Name: "alive", Metrics: HostMetrics{CPUPercent: 30, RAMPercent: 30, DiskPercent: 30}},
	}
	best, err := SelectBestHost(results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if best.Name != "alive" {
		t.Errorf("best = %s, want alive", best.Name)
	}
}

func TestCacheTTL(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	cache := &HostProbeCache{entries: make(map[string]cachedProbe)}
	result := HostProbeResult{Name: "test", Metrics: HostMetrics{CPUPercent: 10}}
	cache.Set("test", result)

	// Should hit cache
	got, ok := cache.Get("test")
	if !ok {
		t.Error("expected cache hit")
	}
	if got.Name != "test" {
		t.Error("wrong cached result")
	}

	// Manually expire
	cache.mu.Lock()
	cache.entries["test"] = cachedProbe{result: result, at: time.Now().Add(-31 * time.Second)}
	cache.mu.Unlock()

	_, ok = cache.Get("test")
	if ok {
		t.Error("expected cache miss after TTL")
	}

	cache.mu.Lock()
	_, stillPresent := cache.entries["test"]
	cache.mu.Unlock()
	if stillPresent {
		t.Error("expected expired cache entry to be purged")
	}
}

func TestCacheMaxEntries_EvictsOldestEntry(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	cache := &HostProbeCache{
		entries:    make(map[string]cachedProbe),
		maxEntries: 2,
	}

	oldest := HostProbeResult{Name: "oldest"}
	newer := HostProbeResult{Name: "newer"}
	cache.mu.Lock()
	cache.entries["oldest"] = cachedProbe{result: oldest, at: time.Now().Add(-10 * time.Second)}
	cache.entries["newer"] = cachedProbe{result: newer, at: time.Now().Add(-5 * time.Second)}
	cache.mu.Unlock()

	cache.Set("latest", HostProbeResult{Name: "latest"})

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 2 {
		t.Fatalf("expected cache size 2, got %d", len(cache.entries))
	}
	if _, ok := cache.entries["oldest"]; ok {
		t.Fatal("expected oldest entry to be evicted")
	}
	if _, ok := cache.entries["newer"]; !ok {
		t.Fatal("expected newer entry to remain")
	}
	if _, ok := cache.entries["latest"]; !ok {
		t.Fatal("expected latest entry to be added")
	}
}

func TestCacheMaxEntries_UpdateExistingKeyDoesNotEvict(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	cache := &HostProbeCache{
		entries:    make(map[string]cachedProbe),
		maxEntries: 1,
	}

	cache.Set("only", HostProbeResult{Name: "first"})
	cache.Set("only", HostProbeResult{Name: "second"})

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 1 {
		t.Fatalf("expected cache size 1, got %d", len(cache.entries))
	}
	if cache.entries["only"].result.Name != "second" {
		t.Fatalf("expected cache update to keep latest value, got %q", cache.entries["only"].result.Name)
	}
}

func TestCacheInvalidation(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	cache := &HostProbeCache{entries: make(map[string]cachedProbe)}
	hosts1 := map[string]HostConfig{"a": {SSHHost: "host-a"}}
	hosts2 := map[string]HostConfig{"b": {SSHHost: "host-b"}}

	cache.InvalidateIfConfigChanged(hosts1)
	cache.Set("a", HostProbeResult{Name: "a"})

	cache.InvalidateIfConfigChanged(hosts2)
	_, ok := cache.Get("a")
	if ok {
		t.Error("expected cache clear after config change")
	}
}

func TestHostConfigHash_ChangesWhenMetadataChanges(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	base := map[string]HostConfig{
		"dev": {SSHHost: "pi@devbox", Description: "main", DefaultPath: "/srv/app"},
	}
	changedDescription := map[string]HostConfig{
		"dev": {SSHHost: "pi@devbox", Description: "secondary", DefaultPath: "/srv/app"},
	}
	changedPath := map[string]HostConfig{
		"dev": {SSHHost: "pi@devbox", Description: "main", DefaultPath: "/srv/other"},
	}

	hashBase := hostConfigHash(base)
	if hashBase == hostConfigHash(changedDescription) {
		t.Fatal("expected description change to alter hash")
	}
	if hashBase == hostConfigHash(changedPath) {
		t.Fatal("expected default path change to alter hash")
	}
}

func TestProbeAllHosts_CacheHitUsesLatestConfig(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, probeCacheTTL)

	hostProbeCache = &HostProbeCache{entries: make(map[string]cachedProbe)}
	prober := &fakeHostProber{}
	ctx := context.Background()

	first := map[string]HostConfig{
		"dev": {SSHHost: "pi@devbox", Description: "old desc"},
	}
	second := map[string]HostConfig{
		"dev": {SSHHost: "pi@devbox", Description: "new desc"},
	}

	firstResults := ProbeAllHosts(ctx, first, prober)
	if len(firstResults) != 1 {
		t.Fatalf("expected 1 result on first probe, got %d", len(firstResults))
	}
	if firstResults[0].Config.Description != "old desc" {
		t.Fatalf("expected old description on first probe, got %q", firstResults[0].Config.Description)
	}
	if prober.calls != 1 {
		t.Fatalf("expected one probe call after first run, got %d", prober.calls)
	}

	secondResults := ProbeAllHosts(ctx, second, prober)
	if len(secondResults) != 1 {
		t.Fatalf("expected 1 result on second probe, got %d", len(secondResults))
	}
	if secondResults[0].Config.Description != "new desc" {
		t.Fatalf("expected updated description from config, got %q", secondResults[0].Config.Description)
	}
	if prober.calls != 1 {
		t.Fatalf("expected cache hit to avoid extra probe, got %d calls", prober.calls)
	}
}

func TestScoring_UsesConfiguredOverloadThreshold(t *testing.T) {
	setHostProbeSettingsForTest(t, 95, probeCacheTTL)

	results := []HostProbeResult{
		{Name: "high", Metrics: HostMetrics{CPUPercent: 90, RAMPercent: 20, DiskPercent: 20}},
	}
	best, err := SelectBestHost(results)
	if err != nil {
		t.Fatalf("expected host to remain selectable with higher threshold: %v", err)
	}
	if best.Name != "high" {
		t.Fatalf("best = %s, want high", best.Name)
	}
}

func TestCache_UsesConfiguredProbeCacheTTL(t *testing.T) {
	setHostProbeSettingsForTest(t, defaultOverloadThreshold, 5*time.Millisecond)

	cache := &HostProbeCache{entries: make(map[string]cachedProbe)}
	cache.Set("fast-expire", HostProbeResult{Name: "fast-expire"})

	time.Sleep(10 * time.Millisecond)

	_, ok := cache.Get("fast-expire")
	if ok {
		t.Fatal("expected entry to expire using configured probe cache ttl")
	}
}
