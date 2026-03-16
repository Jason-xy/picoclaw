package streaming

import (
	"context"
	"errors"
	"testing"

	"github.com/sipeed/picoclaw/pkg/channels"
)

type fakeStreamer struct {
	updates     []string
	events      []channels.StreamEvent
	finalized   []string
	cancels     int
	delivered   bool
	updateErr   error
	eventErr    error
	finalizeErr error
	cancelErr   error
}

func (s *fakeStreamer) Update(ctx context.Context, accumulated string) error {
	s.updates = append(s.updates, accumulated)
	return s.updateErr
}

func (s *fakeStreamer) Finalize(ctx context.Context, finalContent string) (bool, error) {
	s.finalized = append(s.finalized, finalContent)
	return s.delivered, s.finalizeErr
}

func (s *fakeStreamer) Cancel(ctx context.Context) error {
	s.cancels++
	return s.cancelErr
}

func (s *fakeStreamer) AppendEvent(ctx context.Context, evt channels.StreamEvent) error {
	s.events = append(s.events, evt)
	return s.eventErr
}

func TestFanoutStreamerPrimaryDeliveredOnly(t *testing.T) {
	primary := &fakeStreamer{delivered: true}
	mirror := &fakeStreamer{delivered: false}
	streamer := NewFanoutStreamer(primary, mirror)

	delivered, err := streamer.Finalize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if !delivered {
		t.Fatal("Finalize() delivered = false, want true from primary sink")
	}
	if len(mirror.finalized) != 1 {
		t.Fatalf("mirror finalized = %v, want one call", mirror.finalized)
	}
}

func TestFanoutStreamerDetachesBrokenMirror(t *testing.T) {
	primary := &fakeStreamer{}
	mirror := &fakeStreamer{updateErr: errors.New("broken pipe")}
	streamer := NewFanoutStreamer(primary, mirror)

	if err := streamer.Update(context.Background(), "one"); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if err := streamer.Update(context.Background(), "two"); err != nil {
		t.Fatalf("Update() second error = %v", err)
	}

	if got := len(primary.updates); got != 2 {
		t.Fatalf("primary updates = %d, want 2", got)
	}
	if got := len(mirror.updates); got != 1 {
		t.Fatalf("mirror updates = %d, want 1 before detach", got)
	}
}

func TestFanoutStreamerReturnsPrimaryErrors(t *testing.T) {
	primary := &fakeStreamer{finalizeErr: errors.New("primary failed")}
	streamer := NewFanoutStreamer(primary)

	if _, err := streamer.Finalize(context.Background(), "done"); err == nil {
		t.Fatal("Finalize() error = nil, want primary error")
	}
}

func TestFanoutStreamerBroadcastsProgress(t *testing.T) {
	primary := &fakeStreamer{}
	mirror := &fakeStreamer{}
	streamer := NewFanoutStreamer(primary, mirror)

	evt := channels.StreamEvent{Kind: channels.StreamEventToolStart, ToolName: "read_file"}
	if err := streamer.AppendEvent(context.Background(), evt); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}
	if len(primary.events) != 1 || len(mirror.events) != 1 {
		t.Fatalf("events = primary:%d mirror:%d, want 1 each", len(primary.events), len(mirror.events))
	}
}
