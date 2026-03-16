package state

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/channels"
)

type BlockKind string

const (
	BlockKindPlain      BlockKind = "plain"
	BlockKindToolOutput BlockKind = "tool_output"
)

type ContentBlock struct {
	Kind BlockKind
	Text string
}

type ToolStatus string

const (
	ToolStatusQueued   ToolStatus = "queued"
	ToolStatusRunning  ToolStatus = "running"
	ToolStatusComplete ToolStatus = "complete"
	ToolStatusError    ToolStatus = "error"
)

type ToolState struct {
	ToolCallID string
	ToolIndex  int
	ToolName   string
	Status     ToolStatus
}

type State struct {
	initial        string
	frozenBlocks   []ContentBlock
	currentPartial string
	lastPartial    string
	toolStates     []ToolState
}

func New(initial string) *State {
	return &State{initial: strings.TrimSpace(initial)}
}

func (s *State) ApplyAccumulated(accumulated string) bool {
	accumulated = strings.TrimSpace(accumulated)
	if accumulated == "" || accumulated == s.currentPartial {
		return false
	}
	if s.lastPartial != "" && len(accumulated) < len(s.lastPartial) {
		s.appendBlock(ContentBlock{Kind: BlockKindPlain, Text: s.lastPartial})
	}
	s.currentPartial = accumulated
	s.lastPartial = accumulated
	return true
}

func (s *State) ApplyEvent(evt channels.StreamEvent) bool {
	changed := false

	switch evt.Kind {
	case channels.StreamEventToolPlan:
		changed = s.syncToolPlan(evt.Tools)
	case channels.StreamEventToolStart, channels.StreamEventToolResult, channels.StreamEventToolError:
		state := s.upsertToolState(evt.ToolCallID, evt.ToolIndex, evt.ToolName)
		nextStatus := toolStatusFromEvent(evt.Kind)
		if state.Status != nextStatus {
			state.Status = nextStatus
			changed = true
		}
	}

	text := strings.TrimSpace(evt.Text)
	if text == "" {
		return changed
	}

	if strings.TrimSpace(s.currentPartial) != "" {
		s.appendBlock(ContentBlock{Kind: BlockKindPlain, Text: s.currentPartial})
		s.currentPartial = ""
		s.lastPartial = ""
		changed = true
	}

	block := ContentBlock{Kind: BlockKindPlain, Text: text}
	if evt.Kind == channels.StreamEventToolResult || evt.Kind == channels.StreamEventToolError {
		block.Kind = BlockKindToolOutput
	}
	if s.appendBlock(block) {
		changed = true
	}
	return changed
}

func (s *State) Finalize(finalContent, defaultContent string) string {
	finalContent = strings.TrimSpace(finalContent)
	if finalContent == "" {
		finalContent = strings.TrimSpace(s.currentPartial)
	}
	if finalContent == "" {
		finalContent = strings.TrimSpace(defaultContent)
	}
	s.currentPartial = finalContent
	s.lastPartial = finalContent
	return finalContent
}

func (s *State) CurrentPartial() string {
	return strings.TrimSpace(s.currentPartial)
}

func (s *State) RenderMain(formatter func(ContentBlock) string) string {
	parts := make([]string, 0, len(s.frozenBlocks)+1)
	for _, block := range s.frozenBlocks {
		text := renderBlock(block, formatter)
		if text != "" {
			parts = append(parts, text)
		}
	}

	current := strings.TrimSpace(s.currentPartial)
	if current != "" {
		text := renderBlock(ContentBlock{Kind: BlockKindPlain, Text: current}, formatter)
		if text != "" {
			parts = append(parts, text)
		}
	}

	if len(parts) == 0 {
		return s.initial
	}
	return strings.Join(parts, "\n\n")
}

func (s *State) RenderToolStatus(renderer func(ToolState) string) string {
	if len(s.toolStates) == 0 {
		return ""
	}

	lines := make([]string, 0, len(s.toolStates))
	for _, state := range s.toolStates {
		line := strings.TrimSpace(renderer(state))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func (s *State) ToolStates() []ToolState {
	if len(s.toolStates) == 0 {
		return nil
	}
	out := make([]ToolState, len(s.toolStates))
	copy(out, s.toolStates)
	return out
}

func renderBlock(block ContentBlock, formatter func(ContentBlock) string) string {
	if formatter == nil {
		return strings.TrimSpace(block.Text)
	}
	return strings.TrimSpace(formatter(block))
}

func (s *State) appendBlock(block ContentBlock) bool {
	block.Text = strings.TrimSpace(block.Text)
	if block.Text == "" {
		return false
	}
	if n := len(s.frozenBlocks); n > 0 && s.frozenBlocks[n-1] == block {
		return false
	}
	s.frozenBlocks = append(s.frozenBlocks, block)
	return true
}

func (s *State) syncToolPlan(tools []channels.StreamToolRef) bool {
	if len(tools) == 0 {
		return false
	}

	changed := len(s.toolStates) != len(tools)
	existing := make(map[string]ToolState, len(s.toolStates))
	for _, state := range s.toolStates {
		existing[toolStateKey(state.ToolCallID, state.ToolIndex)] = state
	}

	sortedTools := append([]channels.StreamToolRef(nil), tools...)
	for i := 0; i < len(sortedTools)-1; i++ {
		for j := i + 1; j < len(sortedTools); j++ {
			if sortedTools[j].ToolIndex < sortedTools[i].ToolIndex {
				sortedTools[i], sortedTools[j] = sortedTools[j], sortedTools[i]
			}
		}
	}

	next := make([]ToolState, 0, len(sortedTools))
	for _, tool := range sortedTools {
		key := toolStateKey(tool.ToolCallID, tool.ToolIndex)
		state, ok := existing[key]
		if !ok {
			state = ToolState{
				ToolCallID: tool.ToolCallID,
				ToolIndex:  tool.ToolIndex,
				ToolName:   tool.ToolName,
				Status:     ToolStatusQueued,
			}
			changed = true
		}
		if state.ToolName != tool.ToolName {
			state.ToolName = tool.ToolName
			changed = true
		}
		next = append(next, state)
	}
	s.toolStates = next
	return changed
}

func (s *State) upsertToolState(toolCallID string, toolIndex int, toolName string) *ToolState {
	for i := range s.toolStates {
		if s.toolStates[i].ToolCallID == toolCallID && toolCallID != "" {
			if toolName != "" {
				s.toolStates[i].ToolName = toolName
			}
			if toolIndex >= 0 {
				s.toolStates[i].ToolIndex = toolIndex
			}
			return &s.toolStates[i]
		}
		if toolCallID == "" && s.toolStates[i].ToolIndex == toolIndex {
			if toolName != "" {
				s.toolStates[i].ToolName = toolName
			}
			return &s.toolStates[i]
		}
	}

	state := ToolState{
		ToolCallID: toolCallID,
		ToolIndex:  toolIndex,
		ToolName:   toolName,
		Status:     ToolStatusQueued,
	}
	s.toolStates = append(s.toolStates, state)
	return &s.toolStates[len(s.toolStates)-1]
}

func toolStateKey(toolCallID string, toolIndex int) string {
	return fmt.Sprintf("%s:%d", toolCallID, toolIndex)
}

func toolStatusFromEvent(kind channels.StreamEventKind) ToolStatus {
	switch kind {
	case channels.StreamEventToolStart:
		return ToolStatusRunning
	case channels.StreamEventToolError:
		return ToolStatusError
	default:
		return ToolStatusComplete
	}
}
