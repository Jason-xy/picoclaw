package state

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/channels"
)

func TestStateAccumulatesAcrossIterations(t *testing.T) {
	s := New("Thinking...")

	if !s.ApplyAccumulated("part") {
		t.Fatal("expected first partial to change state")
	}
	if !s.ApplyAccumulated("partial") {
		t.Fatal("expected expanded partial to change state")
	}
	if !s.ApplyAccumulated("new") {
		t.Fatal("expected shorter next-iteration partial to change state")
	}

	got := s.RenderMain(nil)
	want := "partial\n\nnew"
	if got != want {
		t.Fatalf("RenderMain() = %q, want %q", got, want)
	}
}

func TestStateProgressEventsStayOrderedAndFoldText(t *testing.T) {
	s := New("Thinking...")

	s.ApplyAccumulated("Planning migration")
	s.ApplyEvent(channels.StreamEvent{
		Kind: channels.StreamEventToolPlan,
		Tools: []channels.StreamToolRef{
			{ToolCallID: "call_b", ToolIndex: 1, ToolName: "copy_auth"},
			{ToolCallID: "call_a", ToolIndex: 0, ToolName: "read_config"},
		},
	})
	s.ApplyEvent(channels.StreamEvent{
		Kind:       channels.StreamEventToolStart,
		ToolCallID: "call_b",
		ToolIndex:  1,
		ToolName:   "copy_auth",
	})
	s.ApplyEvent(channels.StreamEvent{
		Kind:       channels.StreamEventToolResult,
		ToolCallID: "call_a",
		ToolIndex:  0,
		ToolName:   "read_config",
		Text:       "Read picoclaw config",
	})

	mainContent := s.RenderMain(func(block ContentBlock) string {
		if block.Kind == BlockKindToolOutput {
			return "tool:" + block.Text
		}
		return block.Text
	})
	toolStatus := s.RenderToolStatus(func(state ToolState) string {
		return string(state.Status) + ":" + state.ToolName
	})

	if mainContent != "Planning migration\n\ntool:Read picoclaw config" {
		t.Fatalf("mainContent = %q", mainContent)
	}
	wantStatus := "complete:read_config\nrunning:copy_auth"
	if toolStatus != wantStatus {
		t.Fatalf("toolStatus = %q, want %q", toolStatus, wantStatus)
	}
}

func TestStateFinalizeUsesCurrentPartialAndFallback(t *testing.T) {
	s := New("Thinking...")
	s.ApplyAccumulated("draft")

	if got := s.Finalize("", "Done."); got != "draft" {
		t.Fatalf("Finalize() = %q, want %q", got, "draft")
	}
	if got := s.RenderMain(nil); got != "draft" {
		t.Fatalf("RenderMain() = %q, want %q", got, "draft")
	}

	empty := New("Thinking...")
	if got := empty.Finalize("", "Done."); got != "Done." {
		t.Fatalf("Finalize() fallback = %q, want %q", got, "Done.")
	}
}
