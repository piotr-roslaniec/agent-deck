package session

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHRunnerBuildRemoteCommand_QuotesAllDynamicArgs(t *testing.T) {
	runner := &SSHRunner{
		AgentDeckPath: "/opt/agent deck/bin/agent-deck",
		Profile:       "work profile",
	}

	got := runner.buildRemoteCommand("rename", "abc123", "new title; rm -rf /", "quote's here")
	want := "'/opt/agent deck/bin/agent-deck' -p 'work profile' 'rename' 'abc123' 'new title; rm -rf /' 'quote'\\''s here'"
	if got != want {
		t.Fatalf("buildRemoteCommand mismatch\nwant: %s\ngot:  %s", want, got)
	}
}

func TestWrapForSSH_QuotesSSHHost(t *testing.T) {
	inst := NewInstance("ssh-test", "/tmp")
	inst.SSHHost = "user@host -oProxyCommand=bad"
	wrapped := inst.wrapForSSH("agent-deck list --json")

	if !strings.Contains(wrapped, "'user@host -oProxyCommand=bad'") {
		t.Fatalf("expected wrapped SSH host to be single-quoted, got: %s", wrapped)
	}
}

func TestSSHControlPathPattern_UsesHashedToken(t *testing.T) {
	got := sshControlPathPattern()
	if !strings.Contains(got, "%C") {
		t.Fatalf("expected control path to contain %%C, got: %s", got)
	}
	if strings.Contains(got, "%r@%h:%p") {
		t.Fatalf("expected legacy token to be removed, got: %s", got)
	}
	if filepath.Base(got) != "%C" {
		t.Fatalf("expected basename to be %%C, got: %s", filepath.Base(got))
	}
}
