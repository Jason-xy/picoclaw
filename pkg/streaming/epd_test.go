package streaming

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/channels"
	streamstate "github.com/sipeed/picoclaw/pkg/streaming/state"
)

func TestEPDMirrorRenderTextSkipsToolOutputBlocks(t *testing.T) {
	s := &epdMirrorStreamer{
		state: streamstate.New(""),
	}

	if !s.state.ApplyAccumulated("Planning") {
		t.Fatal("expected accumulated text to change state")
	}
	if !s.state.ApplyEvent(channels.StreamEvent{
		Kind: channels.StreamEventToolResult,
		Text: "large tool output",
	}) {
		t.Fatal("expected tool event to change state")
	}
	if !s.state.ApplyAccumulated("Final answer") {
		t.Fatal("expected second accumulated text to change state")
	}

	if got := s.renderTextLocked(); got != "Planning\n\nFinal answer" {
		t.Fatalf("renderTextLocked() = %q, want %q", got, "Planning\n\nFinal answer")
	}
}
