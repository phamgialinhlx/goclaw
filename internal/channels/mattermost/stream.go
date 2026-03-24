package mattermost

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

const streamThrottleInterval = 1000 * time.Millisecond

// mmStream implements channels.ChannelStream for Mattermost.
// It edits the placeholder "Thinking..." post as chunks arrive.
type mmStream struct {
	client     *model.Client4
	channelID  string
	rootID     string
	postID     string    // placeholder post ID
	lastUpdate time.Time // last PatchPost call
	mu         sync.Mutex
}

// Update edits the placeholder with accumulated text, throttled to avoid rate limits.
func (s *mmStream) Update(ctx context.Context, fullText string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.postID == "" || time.Since(s.lastUpdate) < streamThrottleInterval {
		return
	}

	text := fullText
	if len(text) > maxMessageLen {
		text = text[:maxMessageLen] + "..."
	}

	patch := &model.PostPatch{Message: model.NewPointer(text)}
	if _, _, err := s.client.PatchPost(ctx, s.postID, patch); err != nil {
		slog.Debug("mattermost stream chunk update failed", "error", err)
		return
	}

	s.lastUpdate = time.Now()
}

// Stop finalizes the stream. Send() handles the final edit via the placeholder map.
func (s *mmStream) Stop(_ context.Context) error {
	return nil
}

// MessageID returns 0 — Mattermost uses string post IDs, not int message IDs.
// FinalizeStream handles the handoff via type assertion.
func (s *mmStream) MessageID() int {
	return 0
}

// PostID returns the Mattermost post ID for FinalizeStream handoff.
func (s *mmStream) PostID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.postID
}

// StreamEnabled reports whether streaming is active for DMs or groups.
func (c *Channel) StreamEnabled(isGroup bool) bool {
	if isGroup {
		return c.config.GroupStream != nil && *c.config.GroupStream
	}
	return c.config.DMStream != nil && *c.config.DMStream
}

// CreateStream creates a per-run streaming handle for the given chatID.
func (c *Channel) CreateStream(_ context.Context, chatID string, _ bool) (channels.ChannelStream, error) {
	pID, pOK := c.placeholders.Load(chatID)
	if !pOK {
		return &mmStream{
			client:    c.client,
			channelID: extractChannelID(chatID),
			rootID:    extractRootID(chatID),
		}, nil
	}

	return &mmStream{
		client:    c.client,
		channelID: extractChannelID(chatID),
		rootID:    extractRootID(chatID),
		postID:    pID.(string),
	}, nil
}

// ReasoningStreamEnabled returns false — reasoning lane support deferred.
func (c *Channel) ReasoningStreamEnabled() bool { return false }

// FinalizeStream stores the stream's placeholder post ID back into c.placeholders
// so that Send() can edit it with the final formatted response.
func (c *Channel) FinalizeStream(_ context.Context, chatID string, stream channels.ChannelStream) {
	ms, ok := stream.(*mmStream)
	if !ok || ms.postID == "" {
		return
	}
	c.placeholders.Store(chatID, ms.postID)
}

// extractChannelID gets the channel ID from a local_key.
func extractChannelID(localKey string) string {
	if idx := indexOf(localKey, ":thread:"); idx > 0 {
		return localKey[:idx]
	}
	return localKey
}

// extractRootID gets the root_id from a local_key, or "" if not threaded.
func extractRootID(localKey string) string {
	const prefix = ":thread:"
	if idx := indexOf(localKey, prefix); idx > 0 {
		return localKey[idx+len(prefix):]
	}
	return ""
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
