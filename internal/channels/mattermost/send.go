package mattermost

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// Send delivers an outbound message to Mattermost.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("mattermost bot not running")
	}

	channelID := msg.ChatID
	if channelID == "" {
		return fmt.Errorf("empty chat ID for mattermost send")
	}

	placeholderKey := channelID
	if pk := msg.Metadata["placeholder_key"]; pk != "" {
		placeholderKey = pk
	}
	rootID := msg.Metadata["message_thread_id"]

	// Placeholder update (LLM retry notification)
	if msg.Metadata["placeholder_update"] == "true" {
		if pID, ok := c.placeholders.Load(placeholderKey); ok {
			postID := pID.(string)
			patch := &model.PostPatch{Message: model.NewPointer(msg.Content)}
			if _, _, err := c.client.PatchPost(ctx, postID, patch); err != nil {
				slog.Debug("mattermost placeholder update failed", "error", err)
			}
		}
		return nil
	}

	content := msg.Content

	// NO_REPLY: delete placeholder, return
	if content == "" {
		if pID, ok := c.placeholders.Load(placeholderKey); ok {
			c.placeholders.Delete(placeholderKey)
			postID := pID.(string)
			if _, err := c.client.DeletePost(ctx, postID); err != nil {
				slog.Debug("mattermost placeholder delete failed", "error", err)
			}
		}
		return nil
	}

	// Edit placeholder with first chunk, send rest as follow-ups
	if pID, ok := c.placeholders.Load(placeholderKey); ok {
		c.placeholders.Delete(placeholderKey)
		postID := pID.(string)

		editContent, remaining := splitAtLimit(content, maxMessageLen)

		patch := &model.PostPatch{Message: model.NewPointer(editContent)}
		if _, _, err := c.client.PatchPost(ctx, postID, patch); err == nil {
			if remaining != "" {
				return c.sendChunked(ctx, channelID, remaining, rootID)
			}
			return nil
		}
		slog.Warn("mattermost placeholder edit failed, sending new message",
			"channel_id", channelID, "error", fmt.Errorf("patch failed"))
	}

	// Handle media attachments
	for _, media := range msg.Media {
		slog.Debug("mattermost: media attachment (upload not implemented yet)",
			"url", media.URL)
	}

	return c.sendChunked(ctx, channelID, content, rootID)
}

func (c *Channel) sendChunked(ctx context.Context, channelID, content, rootID string) error {
	for len(content) > 0 {
		chunk, rest := splitAtLimit(content, maxMessageLen)
		content = rest

		post := &model.Post{
			ChannelId: channelID,
			Message:   chunk,
			RootId:    rootID,
		}

		if _, _, err := c.client.CreatePost(ctx, post); err != nil {
			return fmt.Errorf("send mattermost message: %w", err)
		}
	}
	return nil
}

// splitAtLimit splits content at maxLen runes, preferring newline boundaries.
func splitAtLimit(content string, maxLen int) (chunk, remaining string) {
	runes := []rune(content)
	if len(runes) <= maxLen {
		return content, ""
	}
	cutAt := maxLen
	candidate := string(runes[:maxLen])
	if idx := strings.LastIndex(candidate, "\n"); idx > len(candidate)/2 {
		return content[:idx+1], content[idx+1:]
	}
	return string(runes[:cutAt]), string(runes[cutAt:])
}
