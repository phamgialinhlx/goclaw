package mattermost

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// HandleMessage overrides BaseChannel to allow messages when the chatID (Mattermost channel)
// is in the allowlist, enabling group-level allowlisting without requiring individual user IDs.
func (c *Channel) HandleMessage(senderID, chatID, content string, mediaPaths []string, metadata map[string]string, peerKind string) {
	if !c.IsAllowed(senderID) && !c.IsAllowed(chatID) {
		return
	}

	userID := senderID
	if idx := strings.IndexByte(senderID, '|'); idx > 0 {
		userID = senderID[:idx]
	}

	var mediaFiles []bus.MediaFile
	for _, p := range mediaPaths {
		mediaFiles = append(mediaFiles, bus.MediaFile{Path: p})
	}

	// Collect contact for processed messages.
	if cc := c.ContactCollector(); cc != nil {
		ctx := store.WithTenantID(context.Background(), c.TenantID())
		cc.EnsureContact(ctx, c.Type(), c.Name(), userID, userID, metadata["username"], "", peerKind)
	}

	c.Bus().PublishInbound(bus.InboundMessage{
		Channel:  c.Name(),
		SenderID: senderID,
		ChatID:   chatID,
		Content:  content,
		Media:    mediaFiles,
		PeerKind: peerKind,
		UserID:   userID,
		Metadata: metadata,
		AgentID:  c.AgentID(),
	})
}

// BlockReplyEnabled returns the per-channel block_reply override.
func (c *Channel) BlockReplyEnabled() *bool { return c.config.BlockReply }

// resolveDisplayName fetches and caches the Mattermost display name for a user ID.
func (c *Channel) resolveDisplayName(ctx context.Context, userID string) string {
	c.userCacheMu.RLock()
	cu, found := c.userCache[userID]
	c.userCacheMu.RUnlock()

	if found && time.Since(cu.fetchedAt) < userCacheTTL {
		return cu.displayName
	}

	user, _, err := c.client.GetUser(ctx, userID, "")
	if err != nil {
		slog.Debug("mattermost: failed to resolve user", "user_id", userID, "error", err)
		return userID
	}

	name := user.GetDisplayName(model.ShowNicknameFullName)
	if name == "" {
		name = user.Username
	}

	c.userCacheMu.Lock()
	c.userCache[userID] = cachedUser{displayName: name, fetchedAt: time.Now()}
	c.userCacheMu.Unlock()

	return name
}

// HealthProbe performs a GetMe call to verify the Mattermost connection is alive.
func (c *Channel) HealthProbe(ctx context.Context) (ok bool, elapsed time.Duration, err error) {
	if c.client == nil {
		return false, 0, fmt.Errorf("mattermost client not initialized (Start() not called)")
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()

	_, _, err = c.client.GetMe(probeCtx, "")
	elapsed = time.Since(start)
	return err == nil, elapsed, err
}
