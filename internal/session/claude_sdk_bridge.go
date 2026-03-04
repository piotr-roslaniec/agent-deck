package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const claudeSDKBridgeBasePath = "/claude-sdk"

type claudeSDKHookStatusFile struct {
	Status    string `json:"status"`
	SessionID string `json:"session_id,omitempty"`
	Event     string `json:"event"`
	Timestamp int64  `json:"ts"`
}

type claudeSDKBridgeUpdate struct {
	status    string
	sessionID string
	event     string
}

// ClaudeSDKBridge is an embedded websocket bridge for Claude SDK mode.
// It receives protocol messages and mirrors them into hook status files.
type ClaudeSDKBridge struct {
	hooksDir string

	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	baseURL  string
	upgrader websocket.Upgrader
}

var (
	defaultClaudeSDKBridgeOnce sync.Once
	defaultClaudeSDKBridge     *ClaudeSDKBridge
)

// GetDefaultClaudeSDKBridge returns the process-wide Claude SDK bridge singleton.
func GetDefaultClaudeSDKBridge() *ClaudeSDKBridge {
	defaultClaudeSDKBridgeOnce.Do(func() {
		defaultClaudeSDKBridge = NewClaudeSDKBridge(GetHooksDir())
	})
	return defaultClaudeSDKBridge
}

// NewClaudeSDKBridge creates a websocket bridge that writes to the provided hooks directory.
func NewClaudeSDKBridge(hooksDir string) *ClaudeSDKBridge {
	return &ClaudeSDKBridge{
		hooksDir: hooksDir,
		upgrader: websocket.Upgrader{
			// Intentionally permissive: the bridge listens on 127.0.0.1 only, so
			// origin filtering adds little security while blocking local SDK clients.
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
	}
}

// URLForInstance ensures the bridge is running and returns the per-instance websocket URL.
func (b *ClaudeSDKBridge) URLForInstance(instanceID string) (string, error) {
	baseURL, err := b.ensureRunning()
	if err != nil {
		return "", err
	}
	return ResolveClaudeSDKURL(baseURL, instanceID), nil
}

func (b *ClaudeSDKBridge) ensureRunning() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.server != nil {
		return b.baseURL, nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("start sdk bridge listener: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(claudeSDKBridgeBasePath+"/", b.handleWS)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	baseURL := fmt.Sprintf("ws://%s%s", listener.Addr().String(), claudeSDKBridgeBasePath)

	b.listener = listener
	b.server = server
	b.baseURL = baseURL

	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			sessionLog.Warn("claude_sdk_bridge_serve_failed", "error", err.Error())
		}
	}()

	return baseURL, nil
}

// Stop shuts down the bridge server.
func (b *ClaudeSDKBridge) Stop() error {
	b.mu.Lock()
	server := b.server
	listener := b.listener
	b.server = nil
	b.listener = nil
	b.baseURL = ""
	b.mu.Unlock()

	if server == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	shutdownErr := server.Shutdown(ctx)
	if listener != nil {
		_ = listener.Close()
	}
	return shutdownErr
}

func (b *ClaudeSDKBridge) handleWS(w http.ResponseWriter, r *http.Request) {
	instanceID := strings.TrimPrefix(r.URL.Path, claudeSDKBridgeBasePath+"/")
	instanceID, err := url.PathUnescape(instanceID)
	if err != nil || !isSafeSDKInstanceID(instanceID) {
		http.Error(w, "invalid instance id", http.StatusBadRequest)
		return
	}

	conn, err := b.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}

		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}

		updates := decodeClaudeSDKPayload(payload)
		for _, update := range updates {
			if update.status == "" {
				continue
			}
			if err := writeClaudeSDKHookStatus(
				b.hooksDir,
				instanceID,
				update.status,
				update.sessionID,
				update.event,
				time.Now(),
			); err != nil {
				sessionLog.Warn(
					"claude_sdk_bridge_write_failed",
					"instance_id", instanceID,
					"error", err.Error(),
				)
			}
		}
	}
}

func decodeClaudeSDKPayload(payload []byte) []claudeSDKBridgeUpdate {
	lines := strings.Split(string(payload), "\n")
	updates := make([]claudeSDKBridgeUpdate, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if update, ok := decodeClaudeSDKLine([]byte(line)); ok {
			updates = append(updates, update)
		}
	}
	return updates
}

func decodeClaudeSDKLine(line []byte) (claudeSDKBridgeUpdate, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return claudeSDKBridgeUpdate{}, false
	}

	msgType := firstRawString(raw, "type", "message_type", "event", "kind")
	msgType = strings.TrimSpace(msgType)
	if msgType == "" {
		return claudeSDKBridgeUpdate{}, false
	}

	status := mapClaudeSDKMessageTypeToStatus(msgType, raw)
	if status == "" {
		return claudeSDKBridgeUpdate{}, false
	}

	sessionID := firstRawString(raw, "session_id", "sessionId", "thread_id", "threadId", "id")
	if sessionID == "" {
		if nested, ok := rawObject(raw, "session"); ok {
			sessionID = firstRawString(nested, "session_id", "sessionId", "id")
		}
	}

	return claudeSDKBridgeUpdate{
		status:    status,
		sessionID: sessionID,
		event:     msgType,
	}, true
}

func mapClaudeSDKMessageTypeToStatus(msgType string, raw map[string]json.RawMessage) string {
	normalized := normalizeClaudeSDKType(msgType)
	if normalized == "" {
		return ""
	}

	// Keep two passes intentionally: exact known types first for stable mappings, then
	// fuzzy matching as a forward-compatible fallback for new/variant SDK event names.
	switch normalized {
	case "assistant", "assistant_message", "assistant_delta":
		return "running"
	case "system_init", "control_request":
		return "waiting"
	case "result", "keep_alive":
		if claudeSDKResultIndicatesDead(raw) {
			return "dead"
		}
		return "idle"
	case "session_end", "shutdown", "closed":
		return "dead"
	}

	switch {
	case strings.HasPrefix(normalized, "assistant_"):
		return "running"
	case strings.Contains(normalized, "control") || strings.Contains(normalized, "permission"):
		return "waiting"
	case strings.Contains(normalized, "system_init"):
		return "waiting"
	case strings.Contains(normalized, "result"):
		if claudeSDKResultIndicatesDead(raw) {
			return "dead"
		}
		return "idle"
	case strings.Contains(normalized, "session_end"), strings.Contains(normalized, "shutdown"), strings.Contains(normalized, "closed"):
		return "dead"
	case strings.Contains(normalized, "keep_alive"), strings.Contains(normalized, "keepalive"):
		if claudeSDKResultIndicatesDead(raw) {
			return "dead"
		}
		return "idle"
	default:
		return ""
	}
}

func claudeSDKResultIndicatesDead(raw map[string]json.RawMessage) bool {
	for _, key := range []string{"status", "state", "outcome", "reason"} {
		if value := normalizeResultState(firstRawString(raw, key)); value != "" {
			if isDeadResultState(value) {
				return true
			}
		}
	}

	if firstRawBool(raw, "is_error", "error", "fatal", "terminated") {
		return true
	}

	if nested, ok := rawObject(raw, "result"); ok {
		for _, key := range []string{"status", "state", "outcome", "reason"} {
			if value := normalizeResultState(firstRawString(nested, key)); value != "" {
				if isDeadResultState(value) {
					return true
				}
			}
		}
		if firstRawBool(nested, "is_error", "error", "fatal", "terminated") {
			return true
		}
	}

	return false
}

func normalizeResultState(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

func isDeadResultState(state string) bool {
	switch state {
	case "error", "failed", "fail", "dead", "terminated", "aborted", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func normalizeClaudeSDKType(msgType string) string {
	s := strings.ToLower(strings.TrimSpace(msgType))
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, ".", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

func firstRawString(raw map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok || len(value) == 0 {
			continue
		}
		var str string
		if err := json.Unmarshal(value, &str); err == nil {
			return strings.TrimSpace(str)
		}
	}
	return ""
}

func firstRawBool(raw map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok || len(value) == 0 {
			continue
		}
		var b bool
		if err := json.Unmarshal(value, &b); err == nil {
			return b
		}
		var str string
		if err := json.Unmarshal(value, &str); err == nil {
			str = strings.ToLower(strings.TrimSpace(str))
			if str == "true" || str == "1" || str == "yes" {
				return true
			}
		}
	}
	return false
}

func rawObject(raw map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	value, ok := raw[key]
	if !ok || len(value) == 0 {
		return nil, false
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(value, &nested); err != nil {
		return nil, false
	}
	return nested, true
}

func isSafeSDKInstanceID(instanceID string) bool {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return false
	}
	if strings.Contains(instanceID, "/") || strings.Contains(instanceID, `\`) {
		return false
	}
	base := filepath.Base(instanceID)
	if base != instanceID || base == "." || base == ".." {
		return false
	}
	return true
}

func writeClaudeSDKHookStatus(
	hooksDir, instanceID, status, sessionID, event string,
	now time.Time,
) error {
	if !isSafeSDKInstanceID(instanceID) {
		return fmt.Errorf("invalid instance id: %q", instanceID)
	}
	status = strings.TrimSpace(status)
	if status == "" {
		return errors.New("status is required")
	}

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create hooks dir: %w", err)
	}

	payload := claudeSDKHookStatusFile{
		Status:    status,
		SessionID: strings.TrimSpace(sessionID),
		Event:     strings.TrimSpace(event),
		Timestamp: now.Unix(),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal hook status: %w", err)
	}

	filePath := filepath.Join(hooksDir, instanceID+".json")
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write temp hook status: %w", err)
	}
	if err := os.Rename(tmpPath, filePath); err != nil {
		return fmt.Errorf("rename hook status: %w", err)
	}
	return nil
}

// ResolveClaudeSDKURL builds a per-instance websocket URL from a base URL.
// If the URL contains "{instance_id}", it is substituted in-place.
// Otherwise the instance id is appended as a final path segment.
func ResolveClaudeSDKURL(baseURL, instanceID string) string {
	baseURL = strings.TrimSpace(baseURL)
	instanceID = strings.TrimSpace(instanceID)
	if baseURL == "" || instanceID == "" {
		return ""
	}

	escapedID := url.PathEscape(instanceID)
	if strings.Contains(baseURL, "{instance_id}") {
		return strings.ReplaceAll(baseURL, "{instance_id}", escapedID)
	}

	return strings.TrimRight(baseURL, "/") + "/" + escapedID
}
