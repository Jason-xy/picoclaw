package streaming

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	streamstate "github.com/sipeed/picoclaw/pkg/streaming/state"
)

const epdDefaultDoneContent = "Done."

const epdProtocolVersion = 1

type epdMirrorStreamer struct {
	cfg     config.EPDStreamMirrorConfig
	channel string
	chatID  string

	state *streamstate.State

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	waitCh   chan error
	stderr   bytes.Buffer
	closed   bool
	detached bool
	waitDone bool
	waitErr  error
}

func newEPDMirrorStreamer(
	ctx context.Context,
	cfg config.EPDStreamMirrorConfig,
	channel, chatID string,
) (channels.Streamer, error) {
	s := &epdMirrorStreamer{
		cfg:     cfg,
		channel: channel,
		chatID:  chatID,
		state:   streamstate.New(""),
		waitCh:  make(chan error, 1),
	}
	if err := s.start(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *epdMirrorStreamer) Update(ctx context.Context, accumulated string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.detached {
		return nil
	}
	if !s.state.ApplyAccumulated(accumulated) {
		return nil
	}
	return s.pushLocked(false)
}

func (s *epdMirrorStreamer) AppendEvent(ctx context.Context, evt channels.StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.detached {
		return nil
	}
	if !s.state.ApplyEvent(evt) {
		return nil
	}
	return s.pushLocked(false)
}

func (s *epdMirrorStreamer) Finalize(ctx context.Context, finalContent string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.detached {
		return false, nil
	}

	s.state.Finalize(finalContent, epdDefaultDoneContent)
	if err := s.sendLocked(epdCommand{
		Command: "finish",
		Text:    s.renderTextLocked(),
		Status:  "DONE",
	}); err != nil {
		return false, s.detachLocked(err)
	}
	return false, s.closeLocked()
}

func (s *epdMirrorStreamer) Cancel(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.detached {
		return nil
	}
	if err := s.sendLocked(epdCommand{
		Command: "cancel",
		Status:  "CANCEL",
	}); err != nil {
		return s.detachLocked(err)
	}
	return s.closeLocked()
}

func (s *epdMirrorStreamer) start(ctx context.Context) error {
	pythonBin := strings.TrimSpace(s.cfg.Python)
	if pythonBin == "" {
		pythonBin = "python3"
	}
	module := strings.TrimSpace(s.cfg.Module)
	if module == "" {
		module = "epd.picoclaw_worker"
	}

	cmd := exec.CommandContext(ctx, pythonBin, "-m", module)
	cmd.Stdout = io.Discard
	cmd.Stderr = &s.stderr
	cmd.Env = buildEPDEnv(s.cfg.ProjectRoot)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	s.cmd = cmd
	s.stdin = stdin
	go func() {
		s.waitCh <- cmd.Wait()
	}()

	if err := s.sendLocked(epdCommand{
		Command:              "open",
		ProtocolVersion:      epdProtocolVersion,
		Title:                s.renderTitle(),
		RenderEveryTokens:    s.cfg.RenderEveryTokens,
		MinPartialIntervalMS: s.cfg.MinPartialIntervalMS,
	}); err != nil {
		_ = s.closeLocked()
		return err
	}
	return nil
}

func buildEPDEnv(projectRoot string) []string {
	env := os.Environ()
	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return env
	}
	if info, err := os.Stat(filepath.Join(projectRoot, "src")); err == nil && info.IsDir() {
		projectRoot = filepath.Join(projectRoot, "src")
	}

	current := os.Getenv("PYTHONPATH")
	pythonPath := projectRoot
	if current != "" {
		pythonPath = projectRoot + string(os.PathListSeparator) + current
	}

	filtered := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, "PYTHONPATH=") {
			continue
		}
		filtered = append(filtered, item)
	}
	filtered = append(filtered, "PYTHONPATH="+pythonPath)
	return filtered
}

func (s *epdMirrorStreamer) pushLocked(force bool) error {
	if err := s.sendLocked(epdCommand{
		Command:         "update",
		ProtocolVersion: epdProtocolVersion,
		Text:            s.renderTextLocked(),
		Status:          s.renderStatusLocked(),
		Force:           force,
	}); err != nil {
		return s.detachLocked(err)
	}
	return nil
}

func (s *epdMirrorStreamer) renderTextLocked() string {
	return s.state.RenderMain(func(block streamstate.ContentBlock) string {
		if block.Kind == streamstate.BlockKindToolOutput {
			return ""
		}
		return block.Text
	})
}

func (s *epdMirrorStreamer) renderStatusLocked() string {
	toolStates := s.state.ToolStates()
	if len(toolStates) == 0 {
		return "THINKING"
	}

	for _, state := range toolStates {
		name := strings.TrimSpace(state.ToolName)
		switch state.Status {
		case streamstate.ToolStatusError:
			return compactStatus("ERR", name)
		case streamstate.ToolStatusRunning:
			return compactStatus("RUN", name)
		}
	}

	for _, state := range toolStates {
		name := strings.TrimSpace(state.ToolName)
		if state.Status == streamstate.ToolStatusQueued {
			return compactStatus("RUN", name)
		}
	}

	return "THINKING"
}

func compactStatus(prefix, name string) string {
	if name == "" {
		return prefix
	}
	return prefix + " " + name
}

func (s *epdMirrorStreamer) renderTitle() string {
	title := strings.TrimSpace(s.cfg.TitleTemplate)
	if title == "" {
		title = "PicoClaw"
	}
	replacer := strings.NewReplacer(
		"{channel}", s.channel,
		"{chat_id}", s.chatID,
	)
	return replacer.Replace(title)
}

func (s *epdMirrorStreamer) sendLocked(cmd epdCommand) error {
	if s.detached {
		return nil
	}
	if err, done := s.pollWaitLocked(); done {
		if err == nil {
			return io.ErrClosedPipe
		}
		return fmt.Errorf("epd worker exited: %w%s", err, s.stderrSuffix())
	}

	payload, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	if _, err := s.stdin.Write(payload); err != nil {
		return fmt.Errorf("write epd worker command %q: %w%s", cmd.Command, err, s.stderrSuffix())
	}
	return nil
}

func (s *epdMirrorStreamer) closeLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true

	var closeErr error
	if !s.detached {
		if err := s.sendLocked(epdCommand{Command: "close"}); err != nil {
			closeErr = err
		}
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.cmd != nil {
		waitErr := s.waitErr
		if !s.waitDone {
			waitErr = <-s.waitCh
			s.waitDone = true
			s.waitErr = waitErr
		}
		if waitErr != nil && closeErr == nil {
			closeErr = fmt.Errorf("epd worker exit: %w%s", waitErr, s.stderrSuffix())
		}
		s.cmd = nil
	}
	return closeErr
}

func (s *epdMirrorStreamer) detachLocked(err error) error {
	s.detached = true
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	return err
}

func (s *epdMirrorStreamer) stderrSuffix() string {
	if s.stderr.Len() == 0 {
		return ""
	}
	return ": " + strings.TrimSpace(s.stderr.String())
}

func (s *epdMirrorStreamer) pollWaitLocked() (error, bool) {
	if s.waitDone {
		return s.waitErr, true
	}
	select {
	case err := <-s.waitCh:
		s.waitDone = true
		s.waitErr = err
		return err, true
	default:
		return nil, false
	}
}

type epdCommand struct {
	Command              string `json:"command"`
	ProtocolVersion      int    `json:"protocol_version,omitempty"`
	Title                string `json:"title,omitempty"`
	Text                 string `json:"text,omitempty"`
	Status               string `json:"status,omitempty"`
	RenderEveryTokens    int    `json:"render_every_tokens,omitempty"`
	MinPartialIntervalMS int    `json:"min_partial_interval_ms,omitempty"`
	Force                bool   `json:"force,omitempty"`
}

var _ channels.Streamer = (*epdMirrorStreamer)(nil)
var _ channels.ProgressStreamer = (*epdMirrorStreamer)(nil)
