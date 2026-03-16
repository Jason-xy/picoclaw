package streaming

import (
	"context"
	"sync"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/logger"
)

type sinkEntry struct {
	streamer channels.Streamer
	progress channels.ProgressStreamer
	active   bool
}

type FanoutStreamer struct {
	mu             sync.Mutex
	primary        sinkEntry
	hasPrimary     bool
	auxiliarySinks []sinkEntry
}

func NewFanoutStreamer(primary channels.Streamer, auxiliary ...channels.Streamer) *FanoutStreamer {
	if primary == nil && len(auxiliary) == 0 {
		return nil
	}

	f := &FanoutStreamer{}
	if primary != nil {
		f.hasPrimary = true
		f.primary = sinkEntry{
			streamer: primary,
			progress: asProgressStreamer(primary),
			active:   true,
		}
	}
	for _, streamer := range auxiliary {
		if streamer == nil {
			continue
		}
		f.auxiliarySinks = append(f.auxiliarySinks, sinkEntry{
			streamer: streamer,
			progress: asProgressStreamer(streamer),
			active:   true,
		})
	}
	if !f.hasPrimary && len(f.auxiliarySinks) == 0 {
		return nil
	}
	return f
}

func (f *FanoutStreamer) Update(ctx context.Context, accumulated string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.hasPrimary && f.primary.active {
		if err := f.primary.streamer.Update(ctx, accumulated); err != nil {
			return err
		}
	}
	for i := range f.auxiliarySinks {
		if !f.auxiliarySinks[i].active {
			continue
		}
		if err := f.auxiliarySinks[i].streamer.Update(ctx, accumulated); err != nil {
			logger.WarnCF("streaming", "auxiliary stream update failed; detaching sink", map[string]any{
				"error": err.Error(),
			})
			f.auxiliarySinks[i].active = false
		}
	}
	return nil
}

func (f *FanoutStreamer) Finalize(ctx context.Context, finalContent string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delivered := false
	if f.hasPrimary && f.primary.active {
		primaryDelivered, err := f.primary.streamer.Finalize(ctx, finalContent)
		if err != nil {
			return false, err
		}
		delivered = primaryDelivered
	}
	for i := range f.auxiliarySinks {
		if !f.auxiliarySinks[i].active {
			continue
		}
		if _, err := f.auxiliarySinks[i].streamer.Finalize(ctx, finalContent); err != nil {
			logger.WarnCF("streaming", "auxiliary stream finalize failed; detaching sink", map[string]any{
				"error": err.Error(),
			})
			f.auxiliarySinks[i].active = false
		}
	}
	return delivered, nil
}

func (f *FanoutStreamer) Cancel(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.hasPrimary && f.primary.active {
		if err := f.primary.streamer.Cancel(ctx); err != nil {
			return err
		}
	}
	for i := range f.auxiliarySinks {
		if !f.auxiliarySinks[i].active {
			continue
		}
		if err := f.auxiliarySinks[i].streamer.Cancel(ctx); err != nil {
			logger.WarnCF("streaming", "auxiliary stream cancel failed; detaching sink", map[string]any{
				"error": err.Error(),
			})
			f.auxiliarySinks[i].active = false
		}
	}
	return nil
}

func (f *FanoutStreamer) AppendEvent(ctx context.Context, evt channels.StreamEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.hasPrimary && f.primary.active && f.primary.progress != nil {
		if err := f.primary.progress.AppendEvent(ctx, evt); err != nil {
			return err
		}
	}
	for i := range f.auxiliarySinks {
		if !f.auxiliarySinks[i].active || f.auxiliarySinks[i].progress == nil {
			continue
		}
		if err := f.auxiliarySinks[i].progress.AppendEvent(ctx, evt); err != nil {
			logger.WarnCF("streaming", "auxiliary stream progress failed; detaching sink", map[string]any{
				"error": err.Error(),
			})
			f.auxiliarySinks[i].active = false
		}
	}
	return nil
}

func asProgressStreamer(streamer channels.Streamer) channels.ProgressStreamer {
	ps, _ := streamer.(channels.ProgressStreamer)
	return ps
}

var _ channels.Streamer = (*FanoutStreamer)(nil)
var _ channels.ProgressStreamer = (*FanoutStreamer)(nil)
