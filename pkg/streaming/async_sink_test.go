package streaming

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/channels"
)

type blockingStreamer struct {
	mu sync.Mutex

	updates   []string
	events    []channels.StreamEvent
	finalized []string

	blockUpdateCh chan struct{}
	updateEntered chan struct{}
}

func (s *blockingStreamer) Update(ctx context.Context, accumulated string) error {
	s.mu.Lock()
	s.updates = append(s.updates, accumulated)
	blockCh := s.blockUpdateCh
	s.mu.Unlock()

	if blockCh != nil {
		if s.updateEntered != nil {
			select {
			case s.updateEntered <- struct{}{}:
			default:
			}
		}
		<-blockCh
	}
	return nil
}

func (s *blockingStreamer) Finalize(ctx context.Context, finalContent string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = append(s.finalized, finalContent)
	return false, nil
}

func (s *blockingStreamer) Cancel(ctx context.Context) error {
	return nil
}

func (s *blockingStreamer) AppendEvent(ctx context.Context, evt channels.StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
	return nil
}

func TestAsyncSinkDropOldestKeepsLatest(t *testing.T) {
	blockCh := make(chan struct{})
	base := &blockingStreamer{blockUpdateCh: blockCh, updateEntered: make(chan struct{}, 1)}
	streamer := NewAsyncSinkStreamer(base, AsyncSinkOptions{
		QueueSize:       1,
		FinalizeTimeout: 2 * time.Second,
		CancelTimeout:   2 * time.Second,
	})

	if err := streamer.Update(context.Background(), "one"); err != nil {
		t.Fatalf("Update(one) error = %v", err)
	}
	select {
	case <-base.updateEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first update to enter sink")
	}
	if err := streamer.Update(context.Background(), "two"); err != nil {
		t.Fatalf("Update(two) error = %v", err)
	}
	if err := streamer.Update(context.Background(), "three"); err != nil {
		t.Fatalf("Update(three) error = %v", err)
	}

	close(blockCh)
	if _, err := streamer.Finalize(context.Background(), "done"); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}

	base.mu.Lock()
	defer base.mu.Unlock()
	if len(base.updates) != 2 {
		t.Fatalf("updates count = %d, want 2", len(base.updates))
	}
	if base.updates[0] != "one" || base.updates[1] != "three" {
		t.Fatalf("updates = %v, want [one three]", base.updates)
	}
}

func TestAsyncSinkForwardsEvents(t *testing.T) {
	base := &blockingStreamer{}
	streamer := NewAsyncSinkStreamer(base, AsyncSinkOptions{
		QueueSize:       2,
		FinalizeTimeout: time.Second,
		CancelTimeout:   time.Second,
	})

	evt := channels.StreamEvent{Kind: channels.StreamEventToolStart, ToolName: "read_file"}
	if err := streamer.(channels.ProgressStreamer).AppendEvent(context.Background(), evt); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}
	if _, err := streamer.Finalize(context.Background(), "done"); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}

	base.mu.Lock()
	defer base.mu.Unlock()
	if len(base.events) != 1 {
		t.Fatalf("events count = %d, want 1", len(base.events))
	}
	if base.events[0].ToolName != "read_file" {
		t.Fatalf("event tool name = %q, want read_file", base.events[0].ToolName)
	}
}

func TestAsyncSinkFinalizeTimeoutDetaches(t *testing.T) {
	base := &blockingStreamer{}
	streamer := NewAsyncSinkStreamer(base, AsyncSinkOptions{
		QueueSize:       1,
		FinalizeTimeout: 5 * time.Millisecond,
		CancelTimeout:   5 * time.Millisecond,
	})

	if err := streamer.Update(context.Background(), "one"); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	deadlineCtx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := streamer.Finalize(deadlineCtx, "done")
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Finalize() took too long: %s", elapsed)
	}
}
