package mcppool

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireScannerBufferReturnsConfiguredSize(t *testing.T) {
	buf := acquireScannerBuffer()
	t.Cleanup(func() { releaseScannerBuffer(buf) })

	if len(buf) != scannerBufferSize {
		t.Fatalf("expected buffer len %d, got %d", scannerBufferSize, len(buf))
	}
	if cap(buf) < scannerBufferSize {
		t.Fatalf("expected buffer cap >= %d, got %d", scannerBufferSize, cap(buf))
	}
}

func TestScannerHandlesLargeMessagesWithPooledBuffer(t *testing.T) {
	// Default bufio.Scanner fails on messages > 64KB
	// MCP responses from tools like context7, firecrawl regularly exceed this
	largeMessage := strings.Repeat("x", 100*1024) // 100KB

	scanner := bufio.NewScanner(strings.NewReader(largeMessage + "\n"))
	scannerBuf := configureScannerWithPooledBuffer(scanner)
	defer releaseScannerBuffer(scannerBuf)

	if !scanner.Scan() {
		t.Fatalf("Scanner should handle 100KB message, got error: %v", scanner.Err())
	}
	if len(scanner.Text()) != 100*1024 {
		t.Errorf("Expected 100KB message, got %d bytes", len(scanner.Text()))
	}
}

func TestDefaultScannerFailsOnLargeMessages(t *testing.T) {
	// Proves the bug: default scanner cannot handle >64KB
	largeMessage := strings.Repeat("x", 100*1024)

	scanner := bufio.NewScanner(strings.NewReader(largeMessage + "\n"))
	// No Buffer() call = default 64KB limit

	if scanner.Scan() {
		t.Fatal("Default scanner should fail on 100KB message (this proves the bug exists)")
	}
	if scanner.Err() == nil {
		t.Fatal("Expected bufio.ErrTooLong error")
	}
}

func TestBroadcastResponsesClosesClientsOnFailure(t *testing.T) {
	// When broadcastResponses exits (MCP died), all client connections
	// should be closed so reconnecting proxies know to retry
	proxy := &SocketProxy{
		name:       "test",
		clients:    make(map[string]net.Conn),
		requestMap: make(map[interface{}]string),
		Status:     StatusRunning,
	}

	// Create a pipe to simulate a client connection
	server, client := net.Pipe()
	proxy.clientsMu.Lock()
	proxy.clients["test-client"] = server
	proxy.clientsMu.Unlock()

	// Simulate what happens after broadcastResponses exits
	proxy.closeAllClientsOnFailure()

	// Client should be closed
	buf := make([]byte, 1)
	_, err := client.Read(buf)
	if err == nil {
		t.Error("Expected client connection to be closed")
	}

	// Clients map should be empty
	proxy.clientsMu.RLock()
	count := len(proxy.clients)
	proxy.clientsMu.RUnlock()
	if count != 0 {
		t.Errorf("Expected 0 clients after failure, got %d", count)
	}
}

func TestSocketProxyHealthCheckAcceptsMatchingErrorResponse(t *testing.T) {
	oldTimeout := socketProxyPingTimeout
	socketProxyPingTimeout = 200 * time.Millisecond
	t.Cleanup(func() { socketProxyPingTimeout = oldTimeout })

	socketPath := filepath.Join(t.TempDir(), "healthcheck.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create test socket: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()

		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			if scanErr := scanner.Err(); scanErr != nil {
				serverDone <- scanErr
				return
			}
			serverDone <- os.ErrInvalid
			return
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			serverDone <- err
			return
		}
		if req.Method != "ping" {
			serverDone <- os.ErrInvalid
			return
		}

		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: map[string]any{
				"code":    -32000,
				"message": "test error response still means server is alive",
			},
		}
		respBytes, err := json.Marshal(resp)
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := conn.Write(append(respBytes, '\n')); err != nil {
			serverDone <- err
			return
		}

		serverDone <- nil
	}()

	proxy := newHealthCheckTestProxy(t, socketPath)
	if err := proxy.HealthCheck(); err != nil {
		t.Fatalf("expected health check success, got error: %v", err)
	}

	if err := <-serverDone; err != nil {
		t.Fatalf("test server error: %v", err)
	}
}

func TestSocketProxyHealthCheckPingTimeout(t *testing.T) {
	oldTimeout := socketProxyPingTimeout
	socketProxyPingTimeout = 50 * time.Millisecond
	t.Cleanup(func() { socketProxyPingTimeout = oldTimeout })

	socketPath := filepath.Join(t.TempDir(), "healthcheck-timeout.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create test socket: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		scanner := bufio.NewScanner(conn)
		_ = scanner.Scan()
		time.Sleep(250 * time.Millisecond)
	}()

	proxy := newHealthCheckTestProxy(t, socketPath)
	err = proxy.HealthCheck()
	if err == nil {
		t.Fatal("expected health check timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out waiting for health ping response") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

func newHealthCheckTestProxy(t *testing.T, socketPath string) *SocketProxy {
	t.Helper()

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("failed to find current process: %v", err)
	}

	return &SocketProxy{
		name:       "test-proxy",
		socketPath: socketPath,
		mcpProcess: &exec.Cmd{Process: proc},
	}
}
