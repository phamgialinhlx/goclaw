package mattermost

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	pairingDebounceTime = 60 * time.Second
	maxMessageLen       = 4000 // Mattermost post limit (16383 max, but keep practical)
	userCacheTTL        = 1 * time.Hour
	healthProbeTimeout  = 2500 * time.Millisecond
)

// Channel connects to Mattermost via WebSocket for event-driven messaging.
type Channel struct {
	*channels.BaseChannel
	client         *model.Client4
	wsClient       *model.WebSocketClient
	config         config.MattermostConfig
	botUserID      string // populated on Start() via GetMe
	botUsername     string
	requireMention bool // require @bot in channels (default true)

	placeholders    sync.Map // localKey -> postID
	dedup           sync.Map // channelID+postID -> time.Time
	threadParticip  sync.Map // channelID+rootID -> time.Time
	reactions       sync.Map // chatID:postID -> *reactionState
	pairingDebounce sync.Map // senderID -> time.Time
	approvedGroups  sync.Map // channelID -> true

	// Read-heavy map: sync.RWMutex + regular map for user display name cache
	userCacheMu sync.RWMutex
	userCache   map[string]cachedUser

	pairingService store.PairingStore
	groupHistory   *channels.PendingHistory
	historyLimit   int
	threadTTL      time.Duration // thread participation expiry (0 = disabled)
	wg             sync.WaitGroup
	cancelFn       context.CancelFunc
}

type cachedUser struct {
	displayName string
	fetchedAt   time.Time
}

// Compile-time interface assertions.
var _ channels.Channel = (*Channel)(nil)
var _ channels.StreamingChannel = (*Channel)(nil)
var _ channels.ReactionChannel = (*Channel)(nil)
var _ channels.BlockReplyChannel = (*Channel)(nil)

// New creates a new Mattermost channel from config.
func New(cfg config.MattermostConfig, msgBus *bus.MessageBus, pairingSvc store.PairingStore, pendingStore store.PendingMessageStore) (*Channel, error) {
	if cfg.ServerURL == "" {
		return nil, fmt.Errorf("mattermost server_url is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("mattermost token is required")
	}

	base := channels.NewBaseChannel(channels.TypeMattermost, msgBus, cfg.AllowFrom)
	base.ValidatePolicy(cfg.DMPolicy, cfg.GroupPolicy)

	requireMention := true
	if cfg.RequireMention != nil {
		requireMention = *cfg.RequireMention
	}

	historyLimit := cfg.HistoryLimit
	if historyLimit == 0 {
		historyLimit = channels.DefaultGroupHistoryLimit
	}

	threadTTL := 24 * time.Hour
	if cfg.ThreadTTL != nil {
		if *cfg.ThreadTTL <= 0 {
			threadTTL = 0
		} else {
			threadTTL = time.Duration(*cfg.ThreadTTL) * time.Hour
		}
	}

	return &Channel{
		BaseChannel:    base,
		config:         cfg,
		requireMention: requireMention,
		pairingService: pairingSvc,
		groupHistory:   channels.MakeHistory(channels.TypeMattermost, pendingStore, base.TenantID()),
		historyLimit:   historyLimit,
		threadTTL:      threadTTL,
		userCache:      make(map[string]cachedUser),
	}, nil
}

// Start connects to the Mattermost server and begins receiving events via WebSocket.
func (c *Channel) Start(ctx context.Context) error {
	c.groupHistory.StartFlusher()
	slog.Info("starting mattermost bot")

	// Create REST client
	c.client = model.NewAPIv4Client(c.config.ServerURL)
	c.client.SetToken(c.config.Token)

	// Verify auth
	me, _, err := c.client.GetMe(ctx, "")
	if err != nil {
		return fmt.Errorf("mattermost auth failed (GetMe): %w", err)
	}
	c.botUserID = me.Id
	c.botUsername = me.Username

	// Build WebSocket URL from server URL
	wsURL := strings.Replace(c.config.ServerURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	wsClient, err := model.NewWebSocketClient4(wsURL, c.config.Token)
	if err != nil {
		return fmt.Errorf("mattermost websocket connect failed: %w", err)
	}
	c.wsClient = wsClient
	c.wsClient.Listen()

	wsCtx, cancel := context.WithCancel(ctx)
	c.cancelFn = cancel

	c.wg.Add(2) // event loop + periodic sweep

	// Goroutine 1: Event loop
	go func() {
		defer c.wg.Done()
		c.eventLoop(wsCtx)
	}()

	// Goroutine 2: Periodic sweep for TTL-based map eviction
	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-wsCtx.Done():
				return
			case <-ticker.C:
				c.sweepMaps()
			}
		}
	}()

	c.SetRunning(true)
	slog.Info("mattermost bot connected", "user_id", c.botUserID, "username", c.botUsername)
	return nil
}

// sweepMaps performs age-based eviction across all TTL-controlled maps.
func (c *Channel) sweepMaps() {
	now := time.Now()

	c.dedup.Range(func(k, v any) bool {
		if now.Sub(v.(time.Time)) > 5*time.Minute {
			c.dedup.Delete(k)
		}
		return true
	})

	if c.threadTTL > 0 {
		c.threadParticip.Range(func(k, v any) bool {
			if now.Sub(v.(time.Time)) > c.threadTTL {
				c.threadParticip.Delete(k)
			}
			return true
		})
	}

	c.userCacheMu.Lock()
	for k, v := range c.userCache {
		if now.Sub(v.fetchedAt) > userCacheTTL {
			delete(c.userCache, k)
		}
	}
	c.userCacheMu.Unlock()

	c.pairingDebounce.Range(func(k, v any) bool {
		if now.Sub(v.(time.Time)) > pairingDebounceTime*10 {
			c.pairingDebounce.Delete(k)
		}
		return true
	})
}

// eventLoop processes WebSocket events from Mattermost.
func (c *Channel) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-c.wsClient.EventChannel:
			if !ok {
				// Channel closed, attempt reconnect
				slog.Warn("mattermost websocket event channel closed, reconnecting")
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Second):
					}
					c.wsClient.Listen()
					if c.wsClient.EventChannel != nil {
						slog.Info("mattermost websocket reconnected")
						break
					}
				}
				continue
			}
			if event == nil {
				continue
			}
			c.handleEvent(ctx, event)
		}
	}
}

func (c *Channel) handleEvent(ctx context.Context, event *model.WebSocketEvent) {
	switch event.EventType() {
	case model.WebsocketEventPosted:
		c.handlePosted(ctx, event)
	}
}

// SetPendingCompaction configures LLM-based auto-compaction for pending messages.
func (c *Channel) SetPendingCompaction(cfg *channels.CompactionConfig) {
	c.groupHistory.SetCompactionConfig(cfg)
}

// Stop gracefully shuts down the Mattermost channel.
func (c *Channel) Stop(_ context.Context) error {
	c.groupHistory.StopFlusher()
	slog.Info("stopping mattermost bot")
	c.SetRunning(false)

	if c.cancelFn != nil {
		c.cancelFn()
	}
	if c.wsClient != nil {
		c.wsClient.Close()
	}

	// Wait for goroutines with timeout
	doneCh := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		slog.Warn("mattermost bot stop timed out after 10s")
	}

	return nil
}
