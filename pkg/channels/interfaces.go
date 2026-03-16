package channels

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/commands"
)

// TypingCapable — channels that can show a typing/thinking indicator.
// StartTyping begins the indicator and returns a stop function.
// The stop function MUST be idempotent and safe to call multiple times.
type TypingCapable interface {
	StartTyping(ctx context.Context, chatID string) (stop func(), err error)
}

// MessageEditor — channels that can edit an existing message.
// messageID is always string; channels convert platform-specific types internally.
type MessageEditor interface {
	EditMessage(ctx context.Context, chatID string, messageID string, content string) error
}

// StreamingCapable — channels that can show progressively updated assistant output.
// BeginStream creates a chat-scoped stream surface that stays alive for the
// duration of one user turn.
type StreamingCapable interface {
	BeginStream(ctx context.Context, chatID string) (Streamer, error)
}

// Streamer receives progressively accumulated assistant text and finalizes the
// delivery surface at the end of the turn.
type Streamer interface {
	Update(ctx context.Context, accumulated string) error
	Finalize(ctx context.Context, finalContent string) (delivered bool, err error)
	Cancel(ctx context.Context) error
}

type StreamEventKind string

const (
	StreamEventToolPlan   StreamEventKind = "tool_plan"
	StreamEventToolStart  StreamEventKind = "tool_start"
	StreamEventToolResult StreamEventKind = "tool_result"
	StreamEventToolError  StreamEventKind = "tool_error"
)

type StreamToolRef struct {
	ToolCallID string
	ToolIndex  int
	ToolName   string
}

type StreamEvent struct {
	Kind       StreamEventKind
	Iteration  int
	ToolCallID string
	ToolIndex  int
	ToolName   string
	Text       string
	Tools      []StreamToolRef
}

// ProgressStreamer is an optional extension for channels that want to fold
// execution progress into the same reply surface used for text streaming.
type ProgressStreamer interface {
	AppendEvent(ctx context.Context, evt StreamEvent) error
}

// ReactionCapable — channels that can add a reaction (e.g. 👀) to an inbound message.
// ReactToMessage adds a reaction and returns an undo function to remove it.
// The undo function MUST be idempotent and safe to call multiple times.
type ReactionCapable interface {
	ReactToMessage(ctx context.Context, chatID, messageID string) (undo func(), err error)
}

// PlaceholderCapable — channels that can send a placeholder message
// (e.g. "Thinking... 💭") that will later be edited to the actual response.
// The channel MUST also implement MessageEditor for the placeholder to be useful.
// SendPlaceholder returns the platform message ID of the placeholder so that
// Manager.preSend can later edit it via MessageEditor.EditMessage.
type PlaceholderCapable interface {
	SendPlaceholder(ctx context.Context, chatID string) (messageID string, err error)
}

// PlaceholderRecorder is injected into channels by Manager.
// Channels call these methods on inbound to register typing/placeholder state.
// Manager uses the registered state on outbound to stop typing and edit placeholders.
type PlaceholderRecorder interface {
	RecordPlaceholder(channel, chatID, placeholderID string)
	RecordTypingStop(channel, chatID string, stop func())
	RecordReactionUndo(channel, chatID string, undo func())
}

// CommandRegistrarCapable is implemented by channels that can register
// command menus with their upstream platform (e.g. Telegram BotCommand).
// Channels that do not support platform-level command menus can ignore it.
type CommandRegistrarCapable interface {
	RegisterCommands(ctx context.Context, defs []commands.Definition) error
}
