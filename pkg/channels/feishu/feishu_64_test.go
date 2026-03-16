//go:build amd64 || arm64 || riscv64 || mips64 || ppc64

package feishu

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func TestExtractContent(t *testing.T) {
	tests := []struct {
		name        string
		messageType string
		rawContent  string
		want        string
	}{
		{
			name:        "text message",
			messageType: "text",
			rawContent:  `{"text": "hello world"}`,
			want:        "hello world",
		},
		{
			name:        "text message invalid JSON",
			messageType: "text",
			rawContent:  `not json`,
			want:        "not json",
		},
		{
			name:        "post message returns raw JSON",
			messageType: "post",
			rawContent:  `{"title": "test post"}`,
			want:        `{"title": "test post"}`,
		},
		{
			name:        "image message returns empty",
			messageType: "image",
			rawContent:  `{"image_key": "img_xxx"}`,
			want:        "",
		},
		{
			name:        "file message with filename",
			messageType: "file",
			rawContent:  `{"file_key": "file_xxx", "file_name": "report.pdf"}`,
			want:        "report.pdf",
		},
		{
			name:        "file message without filename",
			messageType: "file",
			rawContent:  `{"file_key": "file_xxx"}`,
			want:        "",
		},
		{
			name:        "audio message with filename",
			messageType: "audio",
			rawContent:  `{"file_key": "file_xxx", "file_name": "recording.ogg"}`,
			want:        "recording.ogg",
		},
		{
			name:        "media message with filename",
			messageType: "media",
			rawContent:  `{"file_key": "file_xxx", "file_name": "video.mp4"}`,
			want:        "video.mp4",
		},
		{
			name:        "unknown message type returns raw",
			messageType: "sticker",
			rawContent:  `{"sticker_id": "sticker_xxx"}`,
			want:        `{"sticker_id": "sticker_xxx"}`,
		},
		{
			name:        "empty raw content",
			messageType: "text",
			rawContent:  "",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContent(tt.messageType, tt.rawContent)
			if got != tt.want {
				t.Errorf("extractContent(%q, %q) = %q, want %q", tt.messageType, tt.rawContent, got, tt.want)
			}
		})
	}
}

func TestAppendMediaTags(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		messageType string
		mediaRefs   []string
		want        string
	}{
		{
			name:        "no refs returns content unchanged",
			content:     "hello",
			messageType: "image",
			mediaRefs:   nil,
			want:        "hello",
		},
		{
			name:        "empty refs returns content unchanged",
			content:     "hello",
			messageType: "image",
			mediaRefs:   []string{},
			want:        "hello",
		},
		{
			name:        "image with content",
			content:     "check this",
			messageType: "image",
			mediaRefs:   []string{"ref1"},
			want:        "check this [image: photo]",
		},
		{
			name:        "image empty content",
			content:     "",
			messageType: "image",
			mediaRefs:   []string{"ref1"},
			want:        "[image: photo]",
		},
		{
			name:        "audio",
			content:     "listen",
			messageType: "audio",
			mediaRefs:   []string{"ref1"},
			want:        "listen [audio]",
		},
		{
			name:        "media/video",
			content:     "watch",
			messageType: "media",
			mediaRefs:   []string{"ref1"},
			want:        "watch [video]",
		},
		{
			name:        "file",
			content:     "report.pdf",
			messageType: "file",
			mediaRefs:   []string{"ref1"},
			want:        "report.pdf [file]",
		},
		{
			name:        "unknown type",
			content:     "something",
			messageType: "sticker",
			mediaRefs:   []string{"ref1"},
			want:        "something [attachment]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendMediaTags(tt.content, tt.messageType, tt.mediaRefs)
			if got != tt.want {
				t.Errorf(
					"appendMediaTags(%q, %q, %v) = %q, want %q",
					tt.content,
					tt.messageType,
					tt.mediaRefs,
					got,
					tt.want,
				)
			}
		})
	}
}

func TestExtractFeishuSenderID(t *testing.T) {
	strPtr := func(s string) *string { return &s }

	tests := []struct {
		name   string
		sender *larkim.EventSender
		want   string
	}{
		{
			name:   "nil sender",
			sender: nil,
			want:   "",
		},
		{
			name:   "nil sender ID",
			sender: &larkim.EventSender{SenderId: nil},
			want:   "",
		},
		{
			name: "userId preferred",
			sender: &larkim.EventSender{
				SenderId: &larkim.UserId{
					UserId:  strPtr("u_abc123"),
					OpenId:  strPtr("ou_def456"),
					UnionId: strPtr("on_ghi789"),
				},
			},
			want: "u_abc123",
		},
		{
			name: "openId fallback",
			sender: &larkim.EventSender{
				SenderId: &larkim.UserId{
					UserId:  strPtr(""),
					OpenId:  strPtr("ou_def456"),
					UnionId: strPtr("on_ghi789"),
				},
			},
			want: "ou_def456",
		},
		{
			name: "unionId fallback",
			sender: &larkim.EventSender{
				SenderId: &larkim.UserId{
					UserId:  strPtr(""),
					OpenId:  strPtr(""),
					UnionId: strPtr("on_ghi789"),
				},
			},
			want: "on_ghi789",
		},
		{
			name: "all empty strings",
			sender: &larkim.EventSender{
				SenderId: &larkim.UserId{
					UserId:  strPtr(""),
					OpenId:  strPtr(""),
					UnionId: strPtr(""),
				},
			},
			want: "",
		},
		{
			name: "nil userId pointer falls through",
			sender: &larkim.EventSender{
				SenderId: &larkim.UserId{
					UserId:  nil,
					OpenId:  strPtr("ou_def456"),
					UnionId: nil,
				},
			},
			want: "ou_def456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFeishuSenderID(tt.sender)
			if got != tt.want {
				t.Errorf("extractFeishuSenderID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeFeishuStreamMode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want feishuStreamMode
	}{
		{name: "default auto", in: "", want: feishuStreamAuto},
		{name: "cardkit", in: "cardkit", want: feishuStreamCardKit},
		{name: "patch", in: "patch", want: feishuStreamPatch},
		{name: "unknown", in: "weird", want: feishuStreamAuto},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeFeishuStreamMode(tt.in); got != tt.want {
				t.Fatalf("normalizeFeishuStreamMode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildStreamingThinkingCardJSON(t *testing.T) {
	raw, err := buildStreamingThinkingCardJSON("Thinking...")
	if err != nil {
		t.Fatalf("buildStreamingThinkingCardJSON() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal = %v", err)
	}
	configMap, ok := payload["config"].(map[string]any)
	if !ok {
		t.Fatalf("config missing or wrong type: %T", payload["config"])
	}
	if got, ok := configMap["streaming_mode"].(bool); !ok || !got {
		t.Fatalf("streaming_mode = %v, want true", configMap["streaming_mode"])
	}
	body, ok := payload["body"].(map[string]any)
	if !ok {
		t.Fatalf("body missing or wrong type: %T", payload["body"])
	}
	elements, ok := body["elements"].([]any)
	if !ok || len(elements) != 2 {
		t.Fatalf("elements = %v, want 2 elements", body["elements"])
	}
}

func TestBuildStructuredCardJSON_ContainsToolStatusLane(t *testing.T) {
	raw, err := buildStructuredCardJSON("hello", "✅ read_file - complete", false, "hello")
	if err != nil {
		t.Fatalf("buildStructuredCardJSON() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal = %v", err)
	}
	body, ok := payload["body"].(map[string]any)
	if !ok {
		t.Fatalf("body missing or wrong type: %T", payload["body"])
	}
	elements, ok := body["elements"].([]any)
	if !ok || len(elements) != 2 {
		t.Fatalf("elements = %v, want 2 elements", body["elements"])
	}
	second, ok := elements[1].(map[string]any)
	if !ok {
		t.Fatalf("second element wrong type: %T", elements[1])
	}
	if got := second["element_id"]; got != feishuToolStatusElement {
		t.Fatalf("tool status element_id = %v, want %q", got, feishuToolStatusElement)
	}
}

func TestFeishuStreamer_AccumulatesAcrossIterations(t *testing.T) {
	s := &feishuStreamer{initial: "Thinking..."}
	s.cond = sync.NewCond(&s.mu)

	s.mu.Lock()
	if !s.applyAccumulatedLocked("part") {
		t.Fatal("expected first partial to change state")
	}
	if !s.applyAccumulatedLocked("partial") {
		t.Fatal("expected expanded partial to change state")
	}
	if !s.applyAccumulatedLocked("new") {
		t.Fatal("expected shorter next-iteration partial to change state")
	}
	got := s.renderMainLocked()
	s.mu.Unlock()

	want := "partial\n\nnew"
	if got != want {
		t.Fatalf("renderMainLocked() = %q, want %q", got, want)
	}
}

func TestFeishuStreamer_ProgressEventsStayOrderedAndFoldText(t *testing.T) {
	s := &feishuStreamer{initial: "Thinking..."}
	s.cond = sync.NewCond(&s.mu)

	s.mu.Lock()
	s.applyAccumulatedLocked("Planning migration")
	s.applyEventLocked(channels.StreamEvent{
		Kind: channels.StreamEventToolPlan,
		Tools: []channels.StreamToolRef{
			{ToolCallID: "call_b", ToolIndex: 1, ToolName: "copy_auth"},
			{ToolCallID: "call_a", ToolIndex: 0, ToolName: "read_config"},
		},
	})
	s.applyEventLocked(channels.StreamEvent{
		Kind:       channels.StreamEventToolStart,
		ToolCallID: "call_b",
		ToolIndex:  1,
		ToolName:   "copy_auth",
	})
	s.applyEventLocked(channels.StreamEvent{
		Kind:       channels.StreamEventToolResult,
		ToolCallID: "call_a",
		ToolIndex:  0,
		ToolName:   "read_config",
		Text:       "Read picoclaw config",
	})
	mainContent := s.renderMainLocked()
	toolStatus := s.renderToolStatusLocked()
	s.mu.Unlock()

	if mainContent != "Planning migration\n\n```text\nRead picoclaw config\n```" {
		t.Fatalf("mainContent = %q", mainContent)
	}
	wantStatus := "✅ read_config - complete\n🔄 copy_auth - running"
	if toolStatus != wantStatus {
		t.Fatalf("toolStatus = %q, want %q", toolStatus, wantStatus)
	}
}

func TestFormatFeishuToolOutputBlock_EscapesEmbeddedBackticks(t *testing.T) {
	got := formatFeishuToolOutputBlock("line1\n```warn```\nline2")
	want := "````text\nline1\n```warn```\nline2\n````"
	if got != want {
		t.Fatalf("formatFeishuToolOutputBlock() = %q, want %q", got, want)
	}
}

func TestSendPlaceholderSuppressedWhenStreamingEnabled(t *testing.T) {
	ch := &FeishuChannel{
		config: config.FeishuConfig{
			Streaming: config.StreamingConfig{Enabled: true},
			Placeholder: config.PlaceholderConfig{
				Enabled: true,
				Text:    "Thinking...",
			},
		},
	}

	id, err := ch.SendPlaceholder(context.Background(), "oc_chat")
	if err != nil {
		t.Fatalf("SendPlaceholder() error = %v", err)
	}
	if id != "" {
		t.Fatalf("placeholder id = %q, want empty", id)
	}
}

func TestRetryFeishuCall_RebuildsClientOnAuthCode(t *testing.T) {
	ch := &FeishuChannel{
		config: config.FeishuConfig{
			AppID:     "app",
			AppSecret: "secret",
		},
		client: lark.NewClient("app", "secret"),
	}

	var attempts int
	var clients []*lark.Client
	value, err := retryFeishuCall(ch, func(client *lark.Client) (string, int, error) {
		attempts++
		clients = append(clients, client)
		if attempts == 1 {
			return "", feishuAuthErrorCode, nil
		}
		return "ok", 0, nil
	})
	if err != nil {
		t.Fatalf("retryFeishuCall() error = %v", err)
	}
	if value != "ok" {
		t.Fatalf("value = %q, want ok", value)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(clients) != 2 || clients[0] == clients[1] {
		t.Fatal("expected client rebuild between attempts")
	}
}
