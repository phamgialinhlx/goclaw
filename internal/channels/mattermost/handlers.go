package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func (c *Channel) handlePosted(ctx context.Context, event *model.WebSocketEvent) {
	ctx = store.WithTenantID(ctx, c.TenantID())

	postJSON, ok := event.GetData()["post"].(string)
	if !ok {
		return
	}

	var post model.Post
	if err := json.Unmarshal([]byte(postJSON), &post); err != nil {
		slog.Debug("mattermost: failed to unmarshal post", "error", err)
		return
	}

	// Skip bot's own messages
	if post.UserId == c.botUserID {
		return
	}

	// Skip system messages
	if post.Type != "" {
		return
	}

	// Dedup
	dedupKey := post.ChannelId + ":" + post.Id
	if _, loaded := c.dedup.LoadOrStore(dedupKey, time.Now()); loaded {
		return
	}

	senderID := post.UserId
	channelID := post.ChannelId
	content := post.Message

	// Determine if DM by checking channel type from event data
	channelType, _ := event.GetData()["channel_type"].(string)
	isDM := channelType == "D" // D = direct message, G = group message, O = open, P = private
	peerKind := "group"
	if isDM {
		peerKind = "direct"
	}

	// Resolve display name
	displayName := strings.ReplaceAll(c.resolveDisplayName(ctx, senderID), "|", "_")
	compoundSenderID := fmt.Sprintf("%s|%s", senderID, displayName)

	// Policy check
	if isDM {
		if !c.checkDMPolicy(ctx, senderID, channelID) {
			return
		}
	} else {
		if !c.checkGroupPolicy(ctx, senderID, channelID) {
			return
		}
	}

	// Allowlist filter for DMs
	if isDM && !c.IsAllowed(compoundSenderID) {
		slog.Debug("mattermost message rejected by allowlist",
			"user_id", senderID, "display_name", displayName)
		return
	}

	if content == "" {
		return
	}

	// Determine local_key and thread context
	rootID := post.RootId
	localKey := channelID
	if !isDM && rootID != "" {
		localKey = fmt.Sprintf("%s:thread:%s", channelID, rootID)
	}

	// Mention gating in groups (with thread participation cache)
	if !isDM && c.requireMention {
		mentioned := c.isBotMentioned(&post)

		// Thread participation cache
		if !mentioned && rootID != "" && c.threadTTL > 0 {
			participKey := channelID + ":particip:" + rootID
			if lastReply, ok := c.threadParticip.Load(participKey); ok {
				if time.Since(lastReply.(time.Time)) < c.threadTTL {
					mentioned = true
					slog.Debug("mattermost: auto-reply in participated thread",
						"channel_id", channelID, "root_id", rootID)
				} else {
					c.threadParticip.Delete(participKey)
				}
			}
		}

		if !mentioned {
			c.groupHistory.Record(localKey, channels.HistoryEntry{
				Sender:    displayName,
				SenderID:  senderID,
				Body:      content,
				Timestamp: time.Now(),
				MessageID: post.Id,
			}, c.historyLimit)

			if cc := c.ContactCollector(); cc != nil {
				cc.EnsureContact(ctx, c.Type(), c.Name(), senderID, senderID, displayName, "", "group")
			}

			slog.Debug("mattermost group message recorded (no mention)",
				"channel_id", channelID, "user", displayName)
			return
		}
	}

	content = c.stripBotMention(content)
	content = strings.TrimSpace(content)

	if content == "" {
		return
	}

	slog.Debug("mattermost message received",
		"sender_id", senderID, "channel_id", channelID,
		"is_dm", isDM, "preview", channels.Truncate(content, 50))

	// Send "Thinking..." placeholder
	replyRootID := rootID
	if !isDM && replyRootID == "" {
		replyRootID = post.Id // start thread from the triggering message
	}

	placeholder := &model.Post{
		ChannelId: channelID,
		Message:   "Thinking...",
		RootId:    replyRootID,
	}
	placeholderPost, _, err := c.client.CreatePost(ctx, placeholder)
	if err == nil && placeholderPost != nil {
		c.placeholders.Store(localKey, placeholderPost.Id)
	}

	// Build final content with group history context
	finalContent := content
	if peerKind == "group" {
		annotated := fmt.Sprintf("[From: %s]\n%s", displayName, content)
		if c.historyLimit > 0 {
			finalContent = c.groupHistory.BuildContext(localKey, annotated, c.historyLimit)
		} else {
			finalContent = annotated
		}
	}

	metadata := map[string]string{
		"message_id":      post.Id,
		"user_id":         senderID,
		"username":        displayName,
		"channel_id":      channelID,
		"is_dm":           fmt.Sprintf("%t", isDM),
		"local_key":       localKey,
		"placeholder_key": localKey,
	}
	if replyRootID != "" {
		metadata["message_thread_id"] = replyRootID
	}

	c.HandleMessage(compoundSenderID, channelID, finalContent, nil, metadata, peerKind)

	// Record thread participation for auto-reply cache
	if peerKind == "group" {
		if replyRootID != "" {
			participKey := channelID + ":particip:" + replyRootID
			c.threadParticip.Store(participKey, time.Now())
		}
		c.groupHistory.Clear(localKey)
	}
}

// isBotMentioned checks if the post mentions the bot, either via the
// structured props.mentions array (populated by Mattermost's autocomplete)
// or by a plain @username text match (typed manually).
func (c *Channel) isBotMentioned(post *model.Post) bool {
	// Check structured mentions first (reliable — set by Mattermost server)
	if mentions, ok := post.GetProp("mentions").(string); ok && mentions != "" {
		var ids []string
		if err := json.Unmarshal([]byte(mentions), &ids); err == nil {
			if slices.Contains(ids, c.botUserID) {
				return true
			}
		}
	}
	// Fallback: plain text @username match (manual typing, API posts)
	return strings.Contains(post.Message, "@"+c.botUsername)
}

// stripBotMention removes @botUsername from message text.
func (c *Channel) stripBotMention(text string) string {
	return strings.ReplaceAll(text, "@"+c.botUsername, "")
}

// --- Policy checks ---

func (c *Channel) checkDMPolicy(ctx context.Context, senderID, channelID string) bool {
	dmPolicy := c.config.DMPolicy
	if dmPolicy == "" {
		dmPolicy = "pairing"
	}

	switch dmPolicy {
	case "disabled":
		return false
	case "open":
		return true
	case "allowlist":
		return c.HasAllowList() && c.IsAllowed(senderID)
	default: // "pairing"
		if c.pairingService != nil {
			paired, err := c.pairingService.IsPaired(ctx, senderID, c.Name())
			if err != nil {
				slog.Warn("security.pairing_check_failed, assuming paired (fail-open)",
					"sender_id", senderID, "channel", c.Name(), "error", err)
				return true
			}
			if paired {
				return true
			}
		}
		if c.HasAllowList() && c.IsAllowed(senderID) {
			return true
		}
		c.sendPairingReply(ctx, senderID, channelID)
		return false
	}
}

func (c *Channel) checkGroupPolicy(ctx context.Context, senderID, channelID string) bool {
	groupPolicy := c.config.GroupPolicy
	if groupPolicy == "" {
		groupPolicy = "open"
	}

	switch groupPolicy {
	case "disabled":
		return false
	case "allowlist":
		if !c.HasAllowList() {
			return false
		}
		return c.IsAllowed(senderID) || c.IsAllowed(channelID)
	case "pairing":
		if c.HasAllowList() && c.IsAllowed(senderID) {
			return true
		}
		if _, cached := c.approvedGroups.Load(channelID); cached {
			return true
		}
		groupSenderID := fmt.Sprintf("group:%s", channelID)
		if c.pairingService != nil {
			paired, err := c.pairingService.IsPaired(ctx, groupSenderID, c.Name())
			if err != nil {
				slog.Warn("security.pairing_check_failed, assuming paired (fail-open)",
					"group_sender", groupSenderID, "channel", c.Name(), "error", err)
				paired = true
			}
			if paired {
				c.approvedGroups.Store(channelID, true)
				return true
			}
		}
		c.sendPairingReply(ctx, groupSenderID, channelID)
		return false
	default: // "open"
		return true
	}
}

func (c *Channel) sendPairingReply(ctx context.Context, senderID, channelID string) {
	if c.pairingService == nil {
		return
	}

	if lastSent, ok := c.pairingDebounce.Load(senderID); ok {
		if time.Since(lastSent.(time.Time)) < pairingDebounceTime {
			return
		}
	}

	code, err := c.pairingService.RequestPairing(ctx, senderID, c.Name(), channelID, "default", nil)
	if err != nil {
		slog.Warn("mattermost: failed to request pairing code", "error", err)
		return
	}

	var msg string
	if strings.HasPrefix(senderID, "group:") {
		msg = fmt.Sprintf("This channel is not authorized to use this bot.\n\n"+
			"An admin can approve via CLI:\n  goclaw pairing approve %s\n\n"+
			"Or approve via the GoClaw web UI (Pairing section).", code)
	} else {
		msg = fmt.Sprintf("GoClaw: access not configured.\n\nYour Mattermost user ID: %s\n\nPairing code: %s\n\nAsk the bot owner to approve with:\n  goclaw pairing approve %s",
			senderID, code, code)
	}

	post := &model.Post{
		ChannelId: channelID,
		Message:   msg,
	}
	if _, _, err := c.client.CreatePost(ctx, post); err != nil {
		slog.Warn("mattermost: failed to send pairing reply",
			"channel_id", channelID, "error", err)
	}
	c.pairingDebounce.Store(senderID, time.Now())
}
