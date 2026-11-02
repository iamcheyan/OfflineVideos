package media

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingNotifier struct {
	messages []string
	err      error
}

func (n *recordingNotifier) Send(_ context.Context, message string) error {
	n.messages = append(n.messages, message)
	return n.err
}

func TestCompletionMessageIncludesVideoAndNASPath(t *testing.T) {
	message := completionMessage("youtube", "Test video", "abc123", "", "media/Test video.mp4")
	for _, want := range []string{"Test video", "https://www.youtube.com/watch?v=abc123", "media/Test video.mp4"} {
		if !strings.Contains(message, want) {
			t.Fatalf("completion message %q does not contain %q", message, want)
		}
	}
}

func TestHermesTelegramSenderRequiresExplicitTarget(t *testing.T) {
	err := (HermesTelegramSender{}).Send(context.Background(), "test")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected missing configuration error, got %v", err)
	}
}

func TestNotifierErrorIsRepresentable(t *testing.T) {
	n := &recordingNotifier{err: errors.New("offline")}
	if err := n.Send(context.Background(), "message"); err == nil {
		t.Fatal("expected notifier error")
	}
}
