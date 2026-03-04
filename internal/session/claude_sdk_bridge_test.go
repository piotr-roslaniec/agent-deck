package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestResolveClaudeSDKURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		instance string
		want     string
	}{
		{
			name:     "append instance id",
			baseURL:  "ws://127.0.0.1:43123/claude-sdk",
			instance: "inst-1",
			want:     "ws://127.0.0.1:43123/claude-sdk/inst-1",
		},
		{
			name:     "replace placeholder",
			baseURL:  "ws://bridge.local/ws/{instance_id}",
			instance: "inst-2",
			want:     "ws://bridge.local/ws/inst-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveClaudeSDKURL(tt.baseURL, tt.instance)
			if got != tt.want {
				t.Fatalf("ResolveClaudeSDKURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecodeClaudeSDKLine_StatusMapping(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantStatus  string
		wantEvent   string
		wantSession string
	}{
		{
			name:        "assistant maps to running",
			line:        `{"type":"assistant","session_id":"sess-1"}`,
			wantStatus:  "running",
			wantEvent:   "assistant",
			wantSession: "sess-1",
		},
		{
			name:        "control request maps to waiting",
			line:        `{"type":"control_request","session_id":"sess-2"}`,
			wantStatus:  "waiting",
			wantEvent:   "control_request",
			wantSession: "sess-2",
		},
		{
			name:        "result maps to idle",
			line:        `{"type":"result","session_id":"sess-3","status":"ok"}`,
			wantStatus:  "idle",
			wantEvent:   "result",
			wantSession: "sess-3",
		},
		{
			name:        "error result maps to dead",
			line:        `{"type":"result","session_id":"sess-4","status":"error"}`,
			wantStatus:  "dead",
			wantEvent:   "result",
			wantSession: "sess-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := decodeClaudeSDKLine([]byte(tt.line))
			if !ok {
				t.Fatalf("decodeClaudeSDKLine returned ok=false")
			}
			if got.status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", got.status, tt.wantStatus)
			}
			if got.event != tt.wantEvent {
				t.Fatalf("event = %q, want %q", got.event, tt.wantEvent)
			}
			if got.sessionID != tt.wantSession {
				t.Fatalf("sessionID = %q, want %q", got.sessionID, tt.wantSession)
			}
		})
	}
}

func TestClaudeSDKBridge_WebsocketWritesHookStatus(t *testing.T) {
	hooksDir := t.TempDir()
	bridge := NewClaudeSDKBridge(hooksDir)
	defer bridge.Stop()

	wsURL, err := bridge.URLForInstance("inst-bridge")
	if err != nil {
		t.Fatalf("URLForInstance() failed: %v", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial() failed: %v", err)
	}
	defer conn.Close()

	// Send NDJSON payload in one websocket frame: running -> idle.
	payload := []byte("{\"type\":\"assistant\",\"session_id\":\"sdk-session-1\"}\n{\"type\":\"result\",\"session_id\":\"sdk-session-1\",\"status\":\"ok\"}")
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("WriteMessage() failed: %v", err)
	}

	statusPath := filepath.Join(hooksDir, "inst-bridge.json")
	got := waitForHookStatus(t, statusPath, "idle")
	if got.SessionID != "sdk-session-1" {
		t.Fatalf("session_id = %q, want sdk-session-1", got.SessionID)
	}
	if got.Event != "result" {
		t.Fatalf("event = %q, want result", got.Event)
	}
	if got.Timestamp <= 0 {
		t.Fatalf("ts should be unix seconds, got %d", got.Timestamp)
	}
}

func waitForHookStatus(t *testing.T, filePath, wantStatus string) claudeSDKHookStatusFile {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filePath)
		if err == nil && len(data) > 0 {
			var parsed claudeSDKHookStatusFile
			if err := json.Unmarshal(data, &parsed); err == nil && parsed.Status == wantStatus {
				return parsed
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for hook status %q in %s", wantStatus, filePath)
	return claudeSDKHookStatusFile{}
}
