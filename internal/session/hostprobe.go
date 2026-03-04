package session

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// HostMetrics holds resource utilization for a remote host.
type HostMetrics struct {
	CPUPercent  float64 // Load / nproc * 100
	RAMPercent  float64
	DiskPercent float64
}

// HostProbeResult holds the result of probing a single host.
type HostProbeResult struct {
	Name    string
	Config  HostConfig
	Metrics HostMetrics
	Err     error
}

// HostProber is the interface for probing hosts (testable).
type HostProber interface {
	Probe(ctx context.Context, sshHost string) (HostMetrics, error)
}

// SSHHostProber probes hosts via SSH.
type SSHHostProber struct{}

// Probe connects to sshHost and collects CPU/RAM/Disk metrics in a single SSH call.
func (p *SSHHostProber) Probe(ctx context.Context, sshHost string) (HostMetrics, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Single SSH command: load+nproc, ram%, disk%
	probeCmd := `echo "$(awk '{print $1}' /proc/loadavg) $(nproc)" && free | awk '/Mem:/ {printf "%d\n", ($3/$2)*100}' && df -P / | awk 'NR==2 {gsub(/%/,""); print $5}'`

	dir := sshSocketDir()
	_ = os.MkdirAll(dir, 0700)
	controlPath := sshControlPathPattern()

	sshArgs := []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath,
		"-o", "ControlPersist=600",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		sshHost,
		probeCmd,
	}

	cmd := exec.CommandContext(timeoutCtx, "ssh", sshArgs...)
	output, err := cmd.Output()
	if err != nil {
		return HostMetrics{}, fmt.Errorf("ssh probe failed: %w", err)
	}

	return ParseProbeOutput(string(output))
}

// ParseProbeOutput parses the 3-line SSH probe output.
func ParseProbeOutput(output string) (HostMetrics, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 3 {
		return HostMetrics{}, fmt.Errorf("expected 3 lines, got %d", len(lines))
	}

	// Line 1: "load nproc"
	parts := strings.Fields(lines[0])
	if len(parts) < 2 {
		return HostMetrics{}, fmt.Errorf("invalid load line: %s", lines[0])
	}
	load, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return HostMetrics{}, fmt.Errorf("invalid load value: %s", parts[0])
	}
	nproc, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || nproc == 0 {
		return HostMetrics{}, fmt.Errorf("invalid nproc value: %s", parts[1])
	}
	cpuPct := (load / nproc) * 100

	// Line 2: ram%
	ramPct, err := strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
	if err != nil {
		return HostMetrics{}, fmt.Errorf("invalid ram value: %s", lines[1])
	}

	// Line 3: disk%
	diskPct, err := strconv.ParseFloat(strings.TrimSpace(lines[2]), 64)
	if err != nil {
		return HostMetrics{}, fmt.Errorf("invalid disk value: %s", lines[2])
	}

	return HostMetrics{
		CPUPercent:  cpuPct,
		RAMPercent:  ramPct,
		DiskPercent: diskPct,
	}, nil
}

const defaultOverloadThreshold = 80.0
const probeCacheTTL = 30 * time.Second
const probeCacheMaxEntries = 256

var remoteOverloadThresholdProvider = func() float64 {
	return GetRemoteSettings().GetOverloadThreshold()
}

var remoteProbeCacheTTLProvider = func() time.Duration {
	return GetRemoteSettings().GetProbeCacheTTL()
}

var hostProbeCache = &HostProbeCache{
	entries:    make(map[string]cachedProbe),
	maxEntries: probeCacheMaxEntries,
}

type cachedProbe struct {
	result HostProbeResult
	at     time.Time
}

type HostProbeCache struct {
	mu         sync.Mutex
	entries    map[string]cachedProbe
	configHash string
	maxEntries int
}

func (c *HostProbeCache) Get(name string) (HostProbeResult, bool) {
	ttl := remoteProbeCacheTTLProvider()
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[name]
	if !ok {
		return HostProbeResult{}, false
	}
	if time.Since(entry.at) > ttl {
		delete(c.entries, name)
		return HostProbeResult{}, false
	}

	return entry.result, true
}

func (c *HostProbeCache) Set(name string, result HostProbeResult) {
	ttl := remoteProbeCacheTTLProvider()
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if _, exists := c.entries[name]; exists {
		c.entries[name] = cachedProbe{result: result, at: now}
		return
	}

	c.pruneExpiredLocked(now, ttl)
	if len(c.entries) >= c.effectiveMaxEntriesLocked() {
		c.evictOldestLocked()
	}

	c.entries[name] = cachedProbe{result: result, at: now}
}

func (c *HostProbeCache) effectiveMaxEntriesLocked() int {
	if c.maxEntries > 0 {
		return c.maxEntries
	}
	return probeCacheMaxEntries
}

func (c *HostProbeCache) evictOldestLocked() {
	var oldestKey string
	var oldestAt time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.at.Before(oldestAt) {
			oldestKey = key
			oldestAt = entry.at
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

func (c *HostProbeCache) pruneExpiredLocked(now time.Time, ttl time.Duration) {
	for key, entry := range c.entries {
		if now.Sub(entry.at) > ttl {
			delete(c.entries, key)
		}
	}
}

func (c *HostProbeCache) InvalidateIfConfigChanged(hosts map[string]HostConfig) {
	hash := hostConfigHash(hosts)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.configHash != hash {
		c.entries = make(map[string]cachedProbe)
		c.configHash = hash
	}
}

func hostConfigHash(hosts map[string]HostConfig) string {
	names := make([]string, 0, len(hosts))
	for n := range hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		cfg := hosts[n]
		_, _ = fmt.Fprintf(
			h,
			"%d:%s|%d:%s|%d:%s|%d:%s;",
			len(n), n,
			len(cfg.SSHHost), cfg.SSHHost,
			len(cfg.Description), cfg.Description,
			len(cfg.DefaultPath), cfg.DefaultPath,
		)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ProbeAllHosts probes all configured hosts in parallel.
func ProbeAllHosts(ctx context.Context, hosts map[string]HostConfig, prober HostProber) []HostProbeResult {
	hostProbeCache.InvalidateIfConfigChanged(hosts)

	results := make([]HostProbeResult, 0, len(hosts))
	var mu sync.Mutex

	g, gctx := errgroup.WithContext(ctx)
	for name, cfg := range hosts {
		name, cfg := name, cfg
		g.Go(func() error {
			// Check cache first
			if cached, ok := hostProbeCache.Get(name); ok {
				cached.Config = cfg
				mu.Lock()
				results = append(results, cached)
				mu.Unlock()
				return nil
			}

			metrics, err := prober.Probe(gctx, cfg.SSHHost)
			result := HostProbeResult{
				Name:    name,
				Config:  cfg,
				Metrics: metrics,
				Err:     err,
			}
			hostProbeCache.Set(name, result)
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
			return nil // collect-all: don't short-circuit
		})
	}
	_ = g.Wait()
	return results
}

// SelectBestHost picks the least loaded host from probe results.
// Returns error if all hosts are overloaded (any metric >= threshold).
func SelectBestHost(results []HostProbeResult) (HostProbeResult, error) {
	threshold := remoteOverloadThresholdProvider()
	if threshold <= 0 || threshold > 100 {
		threshold = defaultOverloadThreshold
	}

	var healthy []HostProbeResult
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		healthy = append(healthy, r)
	}
	if len(healthy) == 0 {
		return HostProbeResult{}, fmt.Errorf("no reachable hosts")
	}

	// Filter to non-overloaded
	var candidates []HostProbeResult
	for _, r := range healthy {
		peak := maxFloat64(r.Metrics.CPUPercent, maxFloat64(r.Metrics.RAMPercent, r.Metrics.DiskPercent))
		if peak < threshold {
			candidates = append(candidates, r)
		}
	}

	if len(candidates) == 0 {
		// All overloaded — build error message
		var sb strings.Builder
		fmt.Fprintf(&sb, "all hosts overloaded (threshold: %.0f%%):\n", threshold)
		for _, r := range healthy {
			fmt.Fprintf(&sb, "  %s: cpu=%.0f%% ram=%.0f%% disk=%.0f%%\n",
				r.Name, r.Metrics.CPUPercent, r.Metrics.RAMPercent, r.Metrics.DiskPercent)
		}
		sb.WriteString("Use --force or specify a host explicitly")
		return HostProbeResult{}, fmt.Errorf("%s", sb.String())
	}

	// Sort by peak, tiebreak by average
	sort.Slice(candidates, func(i, j int) bool {
		pi := maxFloat64(candidates[i].Metrics.CPUPercent, maxFloat64(candidates[i].Metrics.RAMPercent, candidates[i].Metrics.DiskPercent))
		pj := maxFloat64(candidates[j].Metrics.CPUPercent, maxFloat64(candidates[j].Metrics.RAMPercent, candidates[j].Metrics.DiskPercent))
		if pi != pj {
			return pi < pj
		}
		ai := (candidates[i].Metrics.CPUPercent + candidates[i].Metrics.RAMPercent + candidates[i].Metrics.DiskPercent) / 3
		aj := (candidates[j].Metrics.CPUPercent + candidates[j].Metrics.RAMPercent + candidates[j].Metrics.DiskPercent) / 3
		return ai < aj
	})

	return candidates[0], nil
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
