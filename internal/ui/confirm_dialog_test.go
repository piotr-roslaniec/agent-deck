package ui

import (
	"strings"
	"testing"
)

func TestConfirmDialogDeleteSessionAdoptedDetails(t *testing.T) {
	dialog := NewConfirmDialog()
	dialog.SetSize(100, 30)
	dialog.ShowDeleteSession("s-1", "adopted-session", false, true)

	view := dialog.View()
	if !strings.Contains(view, "local reference will be removed") {
		t.Fatalf("expected adopted delete details in view, got: %q", view)
	}
	if !strings.Contains(view, "remote tmux session stays alive") {
		t.Fatalf("expected remote tmux preservation details in view, got: %q", view)
	}
	if strings.Contains(view, "Any running processes will be killed") {
		t.Fatalf("unexpected non-adopted delete details in adopted flow, got: %q", view)
	}
}

func TestConfirmDialogDeleteSessionStandardDetails(t *testing.T) {
	dialog := NewConfirmDialog()
	dialog.SetSize(100, 30)
	dialog.ShowDeleteSession("s-2", "local-session", false, false)

	view := dialog.View()
	if !strings.Contains(view, "The tmux session will be terminated") {
		t.Fatalf("expected standard delete details in view, got: %q", view)
	}
	if strings.Contains(view, "remote tmux session stays alive") {
		t.Fatalf("unexpected adopted delete details in standard flow, got: %q", view)
	}
}
