//go:build amd64 || arm64 || riscv64 || mips64 || ppc64

package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/logger"
	streamstate "github.com/sipeed/picoclaw/pkg/streaming/state"
)

const (
	feishuCardKitThrottle    = 100 * time.Millisecond
	feishuPatchThrottle      = 1500 * time.Millisecond
	feishuStreamElementID    = "content"
	feishuToolStatusElement  = "tool_status"
	feishuAuthErrorCode      = 99991663
	feishuDefaultDoneContent = "Done."
)

type feishuStreamMode string

const (
	feishuStreamAuto    feishuStreamMode = "auto"
	feishuStreamCardKit feishuStreamMode = "cardkit"
	feishuStreamPatch   feishuStreamMode = "patch"
)

type feishuToolStatus = streamstate.ToolStatus

const (
	feishuToolQueued   feishuToolStatus = streamstate.ToolStatusQueued
	feishuToolRunning  feishuToolStatus = streamstate.ToolStatusRunning
	feishuToolComplete feishuToolStatus = streamstate.ToolStatusComplete
	feishuToolError    feishuToolStatus = streamstate.ToolStatusError
)

type feishuToolState = streamstate.ToolState

type feishuFlushSnapshot struct {
	useCard       bool
	cardID        string
	messageID     string
	mainContent   string
	toolStatus    string
	contentSeq    int
	toolStatusSeq int
}

type feishuStreamer struct {
	channel   *FeishuChannel
	chatID    string
	mode      feishuStreamMode
	initial   string
	useCard   bool
	cardID    string
	messageID string

	mu     sync.Mutex
	cond   *sync.Cond
	closed bool

	sequence int
	state    *streamstate.State

	dirty        bool
	flushRunning bool
	flushTimer   *time.Timer
	lastFlush    time.Time
}

var _ channels.ProgressStreamer = (*feishuStreamer)(nil)

func (c *FeishuChannel) BeginStream(ctx context.Context, chatID string) (channels.Streamer, error) {
	if !c.config.Streaming.Enabled {
		return nil, fmt.Errorf("feishu streaming disabled")
	}
	if chatID == "" {
		return nil, fmt.Errorf("chat ID is empty")
	}

	streamer := &feishuStreamer{
		channel: c,
		chatID:  chatID,
		mode:    normalizeFeishuStreamMode(c.config.Streaming.Mode),
		initial: c.streamingPlaceholderText(),
		state:   streamstate.New(c.streamingPlaceholderText()),
	}
	streamer.cond = sync.NewCond(&streamer.mu)
	if err := streamer.start(ctx); err != nil {
		return nil, err
	}
	return streamer, nil
}

func normalizeFeishuStreamMode(mode string) feishuStreamMode {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "cardkit":
		return feishuStreamCardKit
	case "patch":
		return feishuStreamPatch
	default:
		return feishuStreamAuto
	}
}

func (c *FeishuChannel) streamingPlaceholderText() string {
	text := strings.TrimSpace(c.config.Placeholder.Text)
	if text == "" {
		return "Thinking..."
	}
	return text
}

func (s *feishuStreamer) start(ctx context.Context) error {
	switch s.mode {
	case feishuStreamPatch:
		return s.startPatch(ctx)
	case feishuStreamCardKit:
		if err := s.startCardKit(ctx); err != nil {
			return s.startPatch(ctx)
		}
		return nil
	default:
		if err := s.startCardKit(ctx); err != nil {
			logger.WarnCF("feishu", "CardKit unavailable, falling back to IM patch", map[string]any{
				"chat_id": s.chatID,
				"error":   err.Error(),
			})
			return s.startPatch(ctx)
		}
		return nil
	}
}

func (s *feishuStreamer) startCardKit(ctx context.Context) error {
	cardJSON, err := buildStreamingThinkingCardJSON(s.initial)
	if err != nil {
		return err
	}
	cardID, err := s.channel.createCardEntity(ctx, cardJSON)
	if err != nil {
		return err
	}
	messageID, err := s.channel.sendCardReferenceMessage(ctx, s.chatID, cardID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.useCard = true
	s.cardID = cardID
	s.messageID = messageID
	s.sequence = 1
	s.mu.Unlock()
	return nil
}

func (s *feishuStreamer) startPatch(ctx context.Context) error {
	cardJSON, err := buildStreamingPatchCardJSON(s.initial)
	if err != nil {
		return err
	}
	messageID, err := s.channel.sendInteractiveCardMessage(ctx, s.chatID, cardJSON)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.useCard = false
	s.cardID = ""
	s.messageID = messageID
	s.sequence = 0
	s.mu.Unlock()
	return nil
}

func (s *feishuStreamer) Update(ctx context.Context, accumulated string) error {
	accumulated = strings.TrimSpace(accumulated)
	if accumulated == "" {
		return nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if !s.applyAccumulatedLocked(accumulated) {
		s.mu.Unlock()
		return nil
	}
	s.requestFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *feishuStreamer) AppendEvent(ctx context.Context, evt channels.StreamEvent) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if !s.applyEventLocked(evt) {
		s.mu.Unlock()
		return nil
	}
	s.requestFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *feishuStreamer) Finalize(ctx context.Context, finalContent string) (bool, error) {
	finalContent = strings.TrimSpace(finalContent)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return true, nil
	}
	s.stateLocked().Finalize(finalContent, feishuDefaultDoneContent)
	s.mu.Unlock()

	s.waitForFlush()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return true, nil
	}
	s.closed = true
	s.stopFlushTimerLocked()
	useCard := s.useCard
	cardID := s.cardID
	messageID := s.messageID
	mainContent := s.renderMainLocked()
	toolStatus := s.renderToolStatusLocked()
	if useCard {
		s.sequence++
	}
	settingsSeq := s.sequence
	if useCard {
		s.sequence++
	}
	updateSeq := s.sequence
	s.cond.Broadcast()
	s.mu.Unlock()

	if useCard {
		if err := s.channel.setCardKitStreamingMode(ctx, cardID, false, settingsSeq); err != nil {
			logger.WarnCF("feishu", "CardKit settings update failed during finalize, trying patch fallback", map[string]any{
				"chat_id":    s.chatID,
				"message_id": messageID,
				"card_id":    cardID,
				"error":      err.Error(),
			})
			return s.finalizeViaPatch(ctx, messageID, mainContent, toolStatus)
		}
		if err := s.channel.updateCardKitCard(ctx, cardID, mainContent, toolStatus, updateSeq); err != nil {
			logger.WarnCF("feishu", "CardKit final card update failed, trying patch fallback", map[string]any{
				"chat_id":    s.chatID,
				"message_id": messageID,
				"card_id":    cardID,
				"error":      err.Error(),
			})
			return s.finalizeViaPatch(ctx, messageID, mainContent, toolStatus)
		}
		return true, nil
	}

	return s.finalizeViaPatch(ctx, messageID, mainContent, toolStatus)
}

func (s *feishuStreamer) Cancel(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.dirty = false
	s.stopFlushTimerLocked()
	useCard := s.useCard
	cardID := s.cardID
	if useCard {
		s.sequence++
	}
	sequence := s.sequence
	s.cond.Broadcast()
	s.mu.Unlock()

	s.waitForFlush()

	if !useCard || cardID == "" {
		return nil
	}
	return s.channel.setCardKitStreamingMode(ctx, cardID, false, sequence)
}

func (s *feishuStreamer) applyAccumulatedLocked(accumulated string) bool {
	return s.stateLocked().ApplyAccumulated(accumulated)
}

func (s *feishuStreamer) applyEventLocked(evt channels.StreamEvent) bool {
	return s.stateLocked().ApplyEvent(evt)
}

func (s *feishuStreamer) renderMainLocked() string {
	return s.stateLocked().RenderMain(func(block streamstate.ContentBlock) string {
		if block.Kind == streamstate.BlockKindToolOutput {
			return formatFeishuToolOutputBlock(block.Text)
		}
		return block.Text
	})
}

func (s *feishuStreamer) renderToolStatusLocked() string {
	return s.stateLocked().RenderToolStatus(func(state streamstate.ToolState) string {
		name := strings.TrimSpace(state.ToolName)
		if name == "" {
			return ""
		}
		return fmt.Sprintf("%s %s - %s", feishuToolStatusIcon(state.Status), name, feishuToolStatusLabel(state.Status))
	})
}

func (s *feishuStreamer) stateLocked() *streamstate.State {
	if s.state == nil {
		s.state = streamstate.New(s.initial)
	}
	return s.state
}

func feishuToolStatusIcon(status feishuToolStatus) string {
	switch status {
	case feishuToolRunning:
		return "🔄"
	case feishuToolComplete:
		return "✅"
	case feishuToolError:
		return "❌"
	default:
		return "⏳"
	}
}

func feishuToolStatusLabel(status feishuToolStatus) string {
	switch status {
	case feishuToolRunning:
		return "running"
	case feishuToolComplete:
		return "complete"
	case feishuToolError:
		return "error"
	default:
		return "queued"
	}
}

func (s *feishuStreamer) requestFlushLocked() {
	s.dirty = true
	if s.closed || s.flushRunning || s.flushTimer != nil {
		return
	}

	delay := s.nextFlushDelayLocked()
	if delay <= 0 {
		s.flushRunning = true
		s.cond.Broadcast()
		go s.flushLoop()
		return
	}

	s.flushTimer = time.AfterFunc(delay, s.startScheduledFlush)
	s.cond.Broadcast()
}

func (s *feishuStreamer) nextFlushDelayLocked() time.Duration {
	if s.lastFlush.IsZero() {
		return 0
	}
	throttle := s.throttleLocked()
	since := time.Since(s.lastFlush)
	if since >= throttle {
		return 0
	}
	return throttle - since
}

func (s *feishuStreamer) startScheduledFlush() {
	s.mu.Lock()
	s.flushTimer = nil
	if s.closed || !s.dirty || s.flushRunning {
		s.cond.Broadcast()
		s.mu.Unlock()
		return
	}
	s.flushRunning = true
	s.cond.Broadcast()
	s.mu.Unlock()

	go s.flushLoop()
}

func (s *feishuStreamer) flushLoop() {
	for {
		s.mu.Lock()
		if s.closed || !s.dirty {
			s.flushRunning = false
			s.cond.Broadcast()
			s.mu.Unlock()
			return
		}

		snapshot := s.makeFlushSnapshotLocked()
		s.dirty = false
		s.mu.Unlock()

		if err := s.pushSnapshot(context.Background(), snapshot); err != nil {
			logger.WarnCF("feishu", "stream flush failed", map[string]any{
				"chat_id": s.chatID,
				"error":   err.Error(),
			})
		}

		s.mu.Lock()
		s.lastFlush = time.Now()
		if s.closed {
			s.flushRunning = false
			s.cond.Broadcast()
			s.mu.Unlock()
			return
		}
		if s.dirty {
			delay := s.throttleLocked()
			s.flushRunning = false
			if s.flushTimer == nil {
				s.flushTimer = time.AfterFunc(delay, s.startScheduledFlush)
			}
			s.cond.Broadcast()
			s.mu.Unlock()
			return
		}
		s.flushRunning = false
		s.cond.Broadcast()
		s.mu.Unlock()
		return
	}
}

func (s *feishuStreamer) makeFlushSnapshotLocked() feishuFlushSnapshot {
	mainContent := s.renderMainLocked()
	toolStatus := s.renderToolStatusLocked()
	snapshot := feishuFlushSnapshot{
		useCard:     s.useCard,
		cardID:      s.cardID,
		messageID:   s.messageID,
		mainContent: mainContent,
		toolStatus:  toolStatus,
	}
	if snapshot.useCard {
		s.sequence++
		snapshot.contentSeq = s.sequence
		s.sequence++
		snapshot.toolStatusSeq = s.sequence
	}
	return snapshot
}

func (s *feishuStreamer) pushSnapshot(ctx context.Context, snapshot feishuFlushSnapshot) error {
	if snapshot.useCard {
		if err := s.channel.updateCardKitElement(ctx, snapshot.cardID, feishuStreamElementID, snapshot.mainContent, snapshot.contentSeq); err != nil {
			logger.WarnCF("feishu", "CardKit content update failed, switching to patch mode", map[string]any{
				"chat_id":    s.chatID,
				"message_id": snapshot.messageID,
				"card_id":    snapshot.cardID,
				"error":      err.Error(),
			})
			return s.fallbackToPatchSnapshot(ctx, snapshot)
		}
		if err := s.channel.updateCardKitElement(ctx, snapshot.cardID, feishuToolStatusElement, snapshot.toolStatus, snapshot.toolStatusSeq); err != nil {
			logger.WarnCF("feishu", "CardKit tool status update failed, switching to patch mode", map[string]any{
				"chat_id":    s.chatID,
				"message_id": snapshot.messageID,
				"card_id":    snapshot.cardID,
				"error":      err.Error(),
			})
			return s.fallbackToPatchSnapshot(ctx, snapshot)
		}
		return nil
	}

	cardJSON, err := buildStructuredCardJSON(snapshot.mainContent, snapshot.toolStatus, false, s.initial)
	if err != nil {
		return err
	}
	return s.channel.patchInteractiveCardMessage(ctx, snapshot.messageID, cardJSON)
}

func (s *feishuStreamer) fallbackToPatchSnapshot(ctx context.Context, snapshot feishuFlushSnapshot) error {
	cardJSON, err := buildStructuredCardJSON(snapshot.mainContent, snapshot.toolStatus, false, s.initial)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.useCard = false
	s.cardID = ""
	s.cond.Broadcast()
	s.mu.Unlock()

	return s.channel.patchInteractiveCardMessage(ctx, snapshot.messageID, cardJSON)
}

func (s *feishuStreamer) finalizeViaPatch(
	ctx context.Context,
	messageID, mainContent, toolStatus string,
) (bool, error) {
	cardJSON, err := buildStructuredCardJSON(mainContent, toolStatus, false, mainContent)
	if err != nil {
		return false, err
	}
	if err := s.channel.patchInteractiveCardMessage(ctx, messageID, cardJSON); err != nil {
		return false, err
	}
	return true, nil
}

func (s *feishuStreamer) waitForFlush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.flushRunning || s.flushTimer != nil {
		s.cond.Wait()
	}
}

func (s *feishuStreamer) stopFlushTimerLocked() {
	if s.flushTimer == nil {
		return
	}
	s.flushTimer.Stop()
	s.flushTimer = nil
}

func (s *feishuStreamer) throttleLocked() time.Duration {
	if s.useCard {
		return feishuCardKitThrottle
	}
	return feishuPatchThrottle
}

func buildStreamingThinkingCardJSON(summary string) (string, error) {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"streaming_mode": true,
			"summary": map[string]any{
				"content": summary,
			},
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{
					"tag":        "markdown",
					"content":    "",
					"element_id": feishuStreamElementID,
				},
				{
					"tag":        "markdown",
					"content":    " ",
					"text_size":  "notation",
					"element_id": feishuToolStatusElement,
				},
			},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func buildStreamingPatchCardJSON(summary string) (string, error) {
	return buildStructuredCardJSON(summary, "", false, summary)
}

func buildStructuredCardJSON(mainContent, toolStatus string, streaming bool, summary string) (string, error) {
	mainContent = strings.TrimSpace(mainContent)
	toolStatus = strings.TrimSpace(toolStatus)
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = summarizeCardContent(mainContent)
	}
	configMap := map[string]any{}
	if streaming {
		configMap["streaming_mode"] = true
	}
	if summary != "" {
		configMap["summary"] = map[string]any{
			"content": summary,
		}
	}

	elements := []map[string]any{
		{
			"tag":        "markdown",
			"content":    mainContent,
			"element_id": feishuStreamElementID,
		},
		{
			"tag":        "markdown",
			"content":    statusOrBlank(toolStatus),
			"text_size":  "notation",
			"element_id": feishuToolStatusElement,
		},
	}

	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": elements,
		},
	}
	if len(configMap) > 0 {
		card["config"] = configMap
	}

	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func buildCardKitCard(mainContent, toolStatus string) *larkcardkit.Card {
	cardJSON, _ := buildStructuredCardJSON(mainContent, toolStatus, false, mainContent)
	return larkcardkit.NewCardBuilder().
		Type("card_json").
		Data(cardJSON).
		Build()
}

func statusOrBlank(status string) string {
	if strings.TrimSpace(status) == "" {
		return " "
	}
	return status
}

func summarizeCardContent(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	content = strings.ReplaceAll(content, "\n", " ")
	content = strings.Join(strings.Fields(content), " ")
	if len(content) > 120 {
		return content[:120]
	}
	return content
}

func formatFeishuToolOutputBlock(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	fenceLen := 3
	runLen := 0
	for _, r := range text {
		if r == '`' {
			runLen++
			if runLen >= fenceLen {
				fenceLen = runLen + 1
			}
			continue
		}
		runLen = 0
	}

	fence := strings.Repeat("`", fenceLen)
	return fmt.Sprintf("%stext\n%s\n%s", fence, text, fence)
}

func (c *FeishuChannel) currentClient() *lark.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}

func (c *FeishuChannel) rebuildClient() *lark.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = lark.NewClient(c.config.AppID, c.config.AppSecret)
	return c.client
}

func (c *FeishuChannel) createCardEntity(ctx context.Context, cardJSON string) (string, error) {
	resp, err := c.retryCardKitCreate(ctx, cardJSON)
	if err != nil {
		return "", err
	}
	if !resp.Success() || resp.Data == nil || resp.Data.CardId == nil || *resp.Data.CardId == "" {
		return "", fmt.Errorf("cardkit create failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return *resp.Data.CardId, nil
}

func (c *FeishuChannel) sendCardReferenceMessage(ctx context.Context, chatID, cardID string) (string, error) {
	content := fmt.Sprintf(`{"type":"card","data":{"card_id":"%s"}}`, cardID)
	return c.sendInteractiveCardMessage(ctx, chatID, content)
}

func (c *FeishuChannel) sendInteractiveCardMessage(
	ctx context.Context,
	chatID, cardContent string,
) (string, error) {
	resp, err := c.retryCreateMessage(ctx, chatID, cardContent)
	if err != nil {
		return "", err
	}
	if !resp.Success() || resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
		return "", fmt.Errorf("feishu interactive send failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return *resp.Data.MessageId, nil
}

func (c *FeishuChannel) patchInteractiveCardMessage(
	ctx context.Context,
	messageID, cardContent string,
) error {
	resp, err := c.retryPatchMessage(ctx, messageID, cardContent)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("feishu interactive patch failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *FeishuChannel) updateCardKitElement(
	ctx context.Context,
	cardID, elementID, content string,
	sequence int,
) error {
	resp, err := c.retryCardKitContent(ctx, cardID, elementID, content, sequence)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("cardkit content failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *FeishuChannel) setCardKitStreamingMode(
	ctx context.Context,
	cardID string,
	enabled bool,
	sequence int,
) error {
	settingsJSON := fmt.Sprintf(`{"config":{"streaming_mode":%t}}`, enabled)
	resp, err := c.retryCardKitSettings(ctx, cardID, settingsJSON, sequence)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("cardkit settings failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *FeishuChannel) updateCardKitCard(
	ctx context.Context,
	cardID, mainContent, toolStatus string,
	sequence int,
) error {
	resp, err := c.retryCardKitUpdate(ctx, cardID, buildCardKitCard(mainContent, toolStatus), sequence)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("cardkit update failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *FeishuChannel) retryCreateMessage(
	ctx context.Context,
	chatID, cardContent string,
) (*larkim.CreateMessageResp, error) {
	return retryFeishuCall(c, func(client *lark.Client) (*larkim.CreateMessageResp, int, error) {
		req := larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(larkim.ReceiveIdTypeChatId).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				ReceiveId(chatID).
				MsgType(larkim.MsgTypeInteractive).
				Content(cardContent).
				Build()).
			Build()
		resp, err := client.Im.V1.Message.Create(ctx, req)
		if resp == nil {
			return nil, 0, err
		}
		return resp, resp.Code, err
	})
}

func (c *FeishuChannel) retryPatchMessage(
	ctx context.Context,
	messageID, cardContent string,
) (*larkim.PatchMessageResp, error) {
	return retryFeishuCall(c, func(client *lark.Client) (*larkim.PatchMessageResp, int, error) {
		req := larkim.NewPatchMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewPatchMessageReqBodyBuilder().Content(cardContent).Build()).
			Build()
		resp, err := client.Im.V1.Message.Patch(ctx, req)
		if resp == nil {
			return nil, 0, err
		}
		return resp, resp.Code, err
	})
}

func (c *FeishuChannel) retryCardKitCreate(
	ctx context.Context,
	cardJSON string,
) (*larkcardkit.CreateCardResp, error) {
	return retryFeishuCall(c, func(client *lark.Client) (*larkcardkit.CreateCardResp, int, error) {
		req := larkcardkit.NewCreateCardReqBuilder().
			Body(larkcardkit.NewCreateCardReqBodyBuilder().
				Type("card_json").
				Data(cardJSON).
				Build()).
			Build()
		resp, err := client.Cardkit.V1.Card.Create(ctx, req)
		if resp == nil {
			return nil, 0, err
		}
		return resp, resp.Code, err
	})
}

func (c *FeishuChannel) retryCardKitContent(
	ctx context.Context,
	cardID, elementID, content string,
	sequence int,
) (*larkcardkit.ContentCardElementResp, error) {
	return retryFeishuCall(c, func(client *lark.Client) (*larkcardkit.ContentCardElementResp, int, error) {
		req := larkcardkit.NewContentCardElementReqBuilder().
			CardId(cardID).
			ElementId(elementID).
			Body(larkcardkit.NewContentCardElementReqBodyBuilder().
				Content(statusOrBlank(content)).
				Sequence(sequence).
				Build()).
			Build()
		resp, err := client.Cardkit.V1.CardElement.Content(ctx, req)
		if resp == nil {
			return nil, 0, err
		}
		return resp, resp.Code, err
	})
}

func (c *FeishuChannel) retryCardKitSettings(
	ctx context.Context,
	cardID, settingsJSON string,
	sequence int,
) (*larkcardkit.SettingsCardResp, error) {
	return retryFeishuCall(c, func(client *lark.Client) (*larkcardkit.SettingsCardResp, int, error) {
		req := larkcardkit.NewSettingsCardReqBuilder().
			CardId(cardID).
			Body(larkcardkit.NewSettingsCardReqBodyBuilder().
				Settings(settingsJSON).
				Sequence(sequence).
				Build()).
			Build()
		resp, err := client.Cardkit.V1.Card.Settings(ctx, req)
		if resp == nil {
			return nil, 0, err
		}
		return resp, resp.Code, err
	})
}

func (c *FeishuChannel) retryCardKitUpdate(
	ctx context.Context,
	cardID string,
	card *larkcardkit.Card,
	sequence int,
) (*larkcardkit.UpdateCardResp, error) {
	return retryFeishuCall(c, func(client *lark.Client) (*larkcardkit.UpdateCardResp, int, error) {
		req := larkcardkit.NewUpdateCardReqBuilder().
			CardId(cardID).
			Body(larkcardkit.NewUpdateCardReqBodyBuilder().
				Card(card).
				Sequence(sequence).
				Build()).
			Build()
		resp, err := client.Cardkit.V1.Card.Update(ctx, req)
		if resp == nil {
			return nil, 0, err
		}
		return resp, resp.Code, err
	})
}

func retryFeishuCall[T any](
	channel *FeishuChannel,
	fn func(client *lark.Client) (T, int, error),
) (T, error) {
	resp, code, err := fn(channel.currentClient())
	if code == feishuAuthErrorCode {
		logger.WarnCF("feishu", "auth expired during streaming call, rebuilding client and retrying once", map[string]any{
			"code": code,
		})
		resp, code, err = fn(channel.rebuildClient())
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return resp, nil
}
