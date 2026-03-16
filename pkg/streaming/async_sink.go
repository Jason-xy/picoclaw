package streaming

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sipeed/picoclaw/pkg/channels"
)

type AsyncSinkOptions struct {
	QueueSize       int
	FinalizeTimeout time.Duration
	CancelTimeout   time.Duration
}

func defaultAsyncSinkOptions() AsyncSinkOptions {
	return AsyncSinkOptions{
		QueueSize:       1,
		FinalizeTimeout: 150 * time.Millisecond,
		CancelTimeout:   150 * time.Millisecond,
	}
}

type asyncCommandKind int

const (
	asyncCommandUpdate asyncCommandKind = iota
	asyncCommandEvent
)

type asyncCommand struct {
	kind        asyncCommandKind
	accumulated string
	event       channels.StreamEvent
}

type asyncSinkStreamer struct {
	sink     channels.Streamer
	progress channels.ProgressStreamer
	opts     AsyncSinkOptions

	mu         sync.Mutex
	closed     bool
	detached   atomic.Bool
	dropCount  atomic.Uint64
	queue      chan asyncCommand
	workerDone chan struct{}
}

func NewAsyncSinkStreamer(sink channels.Streamer, opts AsyncSinkOptions) channels.Streamer {
	if sink == nil {
		return nil
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultAsyncSinkOptions().QueueSize
	}
	if opts.FinalizeTimeout <= 0 {
		opts.FinalizeTimeout = defaultAsyncSinkOptions().FinalizeTimeout
	}
	if opts.CancelTimeout <= 0 {
		opts.CancelTimeout = defaultAsyncSinkOptions().CancelTimeout
	}

	s := &asyncSinkStreamer{
		sink:       sink,
		progress:   asProgressStreamer(sink),
		opts:       opts,
		queue:      make(chan asyncCommand, opts.QueueSize),
		workerDone: make(chan struct{}),
	}
	go s.worker()
	return s
}

func (s *asyncSinkStreamer) Update(ctx context.Context, accumulated string) error {
	if s.isStopped() {
		return nil
	}
	s.enqueue(asyncCommand{kind: asyncCommandUpdate, accumulated: accumulated})
	return nil
}

func (s *asyncSinkStreamer) AppendEvent(ctx context.Context, evt channels.StreamEvent) error {
	if s.isStopped() {
		return nil
	}
	s.enqueue(asyncCommand{kind: asyncCommandEvent, event: evt})
	return nil
}

func (s *asyncSinkStreamer) Finalize(ctx context.Context, finalContent string) (bool, error) {
	s.closeQueue()
	if !s.waitWorker(s.opts.FinalizeTimeout) {
		s.detached.Store(true)
		return false, nil
	}
	if s.detached.Load() {
		return false, nil
	}
	return s.callFinalizeWithTimeout(ctx, finalContent, s.opts.FinalizeTimeout)
}

func (s *asyncSinkStreamer) Cancel(ctx context.Context) error {
	s.closeQueue()
	if !s.waitWorker(s.opts.CancelTimeout) {
		s.detached.Store(true)
		return nil
	}
	if s.detached.Load() {
		return nil
	}
	return s.callCancelWithTimeout(ctx, s.opts.CancelTimeout)
}

func (s *asyncSinkStreamer) worker() {
	defer close(s.workerDone)
	for cmd := range s.queue {
		if s.detached.Load() {
			return
		}

		var err error
		switch cmd.kind {
		case asyncCommandUpdate:
			err = s.sink.Update(context.Background(), cmd.accumulated)
		case asyncCommandEvent:
			if s.progress != nil {
				err = s.progress.AppendEvent(context.Background(), cmd.event)
			}
		}
		if err != nil {
			s.detached.Store(true)
			return
		}
	}
}

func (s *asyncSinkStreamer) enqueue(cmd asyncCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.detached.Load() {
		return
	}

	select {
	case s.queue <- cmd:
		return
	default:
	}

	select {
	case <-s.queue:
		s.dropCount.Add(1)
	default:
	}

	select {
	case s.queue <- cmd:
	default:
		s.dropCount.Add(1)
	}
}

func (s *asyncSinkStreamer) isStopped() bool {
	if s.detached.Load() {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *asyncSinkStreamer) closeQueue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.queue)
}

func (s *asyncSinkStreamer) waitWorker(timeout time.Duration) bool {
	if timeout <= 0 {
		<-s.workerDone
		return true
	}

	select {
	case <-s.workerDone:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *asyncSinkStreamer) callFinalizeWithTimeout(
	ctx context.Context,
	finalContent string,
	timeout time.Duration,
) (bool, error) {
	resultCh := make(chan struct {
		delivered bool
		err       error
	}, 1)
	go func() {
		delivered, err := s.sink.Finalize(ctx, finalContent)
		resultCh <- struct {
			delivered bool
			err       error
		}{
			delivered: delivered,
			err:       err,
		}
	}()

	if timeout <= 0 {
		result := <-resultCh
		return result.delivered, result.err
	}

	select {
	case result := <-resultCh:
		return result.delivered, result.err
	case <-time.After(timeout):
		s.detached.Store(true)
		go s.backgroundClose(ctx)
		return false, nil
	}
}

func (s *asyncSinkStreamer) callCancelWithTimeout(ctx context.Context, timeout time.Duration) error {
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- s.sink.Cancel(ctx)
	}()

	if timeout <= 0 {
		return <-resultCh
	}

	select {
	case err := <-resultCh:
		return err
	case <-time.After(timeout):
		s.detached.Store(true)
		go s.backgroundClose(ctx)
		return nil
	}
}

func (s *asyncSinkStreamer) backgroundClose(ctx context.Context) {
	_ = s.sink.Cancel(ctx)
}

var _ channels.Streamer = (*asyncSinkStreamer)(nil)
var _ channels.ProgressStreamer = (*asyncSinkStreamer)(nil)
