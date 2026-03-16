package streaming

import (
	"context"
	"time"

	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	defaultMirrorQueueSize       = 1
	defaultMirrorFinalizeTimeout = 5 * time.Second
	defaultMirrorCancelTimeout   = 2 * time.Second
)

type MirrorManager struct {
	cfg *config.Config
}

func NewMirrorManager(cfg *config.Config) *MirrorManager {
	if cfg == nil {
		return nil
	}
	return &MirrorManager{cfg: cfg}
}

func (m *MirrorManager) BeginStream(ctx context.Context, channel, chatID string) []channels.Streamer {
	if m == nil || m.cfg == nil {
		return nil
	}

	var streamers []channels.Streamer
	if m.cfg.StreamMirrors.EPD.Enabled {
		streamer, err := newEPDMirrorStreamer(ctx, m.cfg.StreamMirrors.EPD, channel, chatID)
		if err != nil {
			logger.WarnCF("streaming", "EPD mirror unavailable; continuing without it", map[string]any{
				"channel": channel,
				"chat_id": chatID,
				"error":   err.Error(),
			})
		} else {
			streamers = append(streamers, NewAsyncSinkStreamer(streamer, AsyncSinkOptions{
				QueueSize:       defaultMirrorQueueSize,
				FinalizeTimeout: defaultMirrorFinalizeTimeout,
				CancelTimeout:   defaultMirrorCancelTimeout,
			}))
		}
	}
	return streamers
}
