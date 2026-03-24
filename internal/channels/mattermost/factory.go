package mattermost

import (
	"encoding/json"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// mmCreds maps the credentials JSON from the channel_instances table.
type mmCreds struct {
	ServerURL string `json:"server_url"`
	Token     string `json:"token"`
}

// mmInstanceConfig maps the non-secret config JSONB from the channel_instances table.
type mmInstanceConfig struct {
	DMPolicy       string   `json:"dm_policy,omitempty"`
	GroupPolicy    string   `json:"group_policy,omitempty"`
	AllowFrom      []string `json:"allow_from,omitempty"`
	RequireMention *bool    `json:"require_mention,omitempty"`
	HistoryLimit   int      `json:"history_limit,omitempty"`
	DMStream       *bool    `json:"dm_stream,omitempty"`
	GroupStream    *bool    `json:"group_stream,omitempty"`
	ReactionLevel  string   `json:"reaction_level,omitempty"`
	BlockReply     *bool    `json:"block_reply,omitempty"`
	ThreadTTL      *int     `json:"thread_ttl,omitempty"`
}

// Factory creates a Mattermost channel from DB instance data.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

	var c mmCreds
	if len(creds) > 0 {
		if err := json.Unmarshal(creds, &c); err != nil {
			return nil, fmt.Errorf("decode mattermost credentials: %w", err)
		}
	}
	if c.ServerURL == "" {
		return nil, fmt.Errorf("mattermost server_url is required")
	}
	if c.Token == "" {
		return nil, fmt.Errorf("mattermost token is required")
	}

	var ic mmInstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("decode mattermost config: %w", err)
		}
	}

	mmCfg := config.MattermostConfig{
		Enabled:        true,
		ServerURL:      c.ServerURL,
		Token:          c.Token,
		AllowFrom:      ic.AllowFrom,
		DMPolicy:       ic.DMPolicy,
		GroupPolicy:    ic.GroupPolicy,
		RequireMention: ic.RequireMention,
		HistoryLimit:   ic.HistoryLimit,
		DMStream:       ic.DMStream,
		GroupStream:    ic.GroupStream,
		ReactionLevel:  ic.ReactionLevel,
		BlockReply:     ic.BlockReply,
		ThreadTTL:      ic.ThreadTTL,
	}

	// Secure default: DB instances default to "pairing" for groups.
	if mmCfg.GroupPolicy == "" {
		mmCfg.GroupPolicy = "pairing"
	}

	ch, err := New(mmCfg, msgBus, pairingSvc, nil)
	if err != nil {
		return nil, err
	}
	ch.SetName(name)
	return ch, nil
}

// FactoryWithPendingStore returns a ChannelFactory with persistent history support.
func FactoryWithPendingStore(pendingStore store.PendingMessageStore) channels.ChannelFactory {
	return func(name string, creds json.RawMessage, cfg json.RawMessage,
		msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

		var c mmCreds
		if len(creds) > 0 {
			if err := json.Unmarshal(creds, &c); err != nil {
				return nil, fmt.Errorf("decode mattermost credentials: %w", err)
			}
		}
		if c.ServerURL == "" {
			return nil, fmt.Errorf("mattermost server_url is required")
		}
		if c.Token == "" {
			return nil, fmt.Errorf("mattermost token is required")
		}

		var ic mmInstanceConfig
		if len(cfg) > 0 {
			if err := json.Unmarshal(cfg, &ic); err != nil {
				return nil, fmt.Errorf("decode mattermost config: %w", err)
			}
		}

		mmCfg := config.MattermostConfig{
			Enabled:        true,
			ServerURL:      c.ServerURL,
			Token:          c.Token,
			AllowFrom:      ic.AllowFrom,
			DMPolicy:       ic.DMPolicy,
			GroupPolicy:    ic.GroupPolicy,
			RequireMention: ic.RequireMention,
			HistoryLimit:   ic.HistoryLimit,
			DMStream:       ic.DMStream,
			GroupStream:    ic.GroupStream,
			ReactionLevel:  ic.ReactionLevel,
			BlockReply:     ic.BlockReply,
			ThreadTTL:      ic.ThreadTTL,
		}

		if mmCfg.GroupPolicy == "" {
			mmCfg.GroupPolicy = "pairing"
		}

		ch, err := New(mmCfg, msgBus, pairingSvc, pendingStore)
		if err != nil {
			return nil, err
		}
		ch.SetName(name)
		return ch, nil
	}
}
