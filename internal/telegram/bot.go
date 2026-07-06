// Package telegram implements the long-polling Telegram bot that hands
// .conf files to members of a single configured group. The bot is
// intentionally separate from the admin web UI: a Telegram user is NOT
// an admin and vice versa. See AGENTS.md invariant.
package telegram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/erazumov/wgserver/internal/ipam"
	"github.com/erazumov/wgserver/internal/store"
	"github.com/erazumov/wgserver/internal/wg"
)

// KeyPairFunc returns (privateKey, publicKey, error). Production
// adapters wrap wg.GenerateKeyPair; tests pass a stub.
type KeyPairFunc func() (string, string, error)

// Bot is the long-polling bot. Run it with Run(ctx) under the same
// context as the HTTP server and the sync loop.
type Bot struct {
	DB         *sql.DB
	Sender     Sender
	GenKeyPair KeyPairFunc
	Logger     *log.Logger

	GroupChatID  int64
	PerUserQuota int

	// Server-side .conf pieces. Mirrors config.ClientsConfig; the bot
	// does not import config to keep the package self-contained.
	ServerPublicKey string
	ServerEndpoint  string
	DNSServers      []string
	CIDR            string
	ServerAddr      string

	// PollTimeout is the long-poll value passed to getUpdates
	// (Telegram holds the request open for up to N seconds). Should
	// be ≤30s; 25s is the conventional choice.
	PollTimeout time.Duration
}

// Run long-polls the Bot API until ctx is cancelled. Each returned
// update is processed by handleUpdate. Errors from a single update are
// logged and do not stop the loop. Errors from the network layer
// (GetUpdates) are retried on the next tick.
func (b *Bot) Run(ctx context.Context) {
	if b.Logger == nil {
		b.Logger = log.Default()
	}
	if b.PollTimeout <= 0 {
		b.PollTimeout = 25 * time.Second
	}
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		updates, err := b.Sender.GetUpdates(ctx, int(offset+1), int(b.PollTimeout.Seconds()))
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			b.Logger.Printf("telegram: getUpdates: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if err := b.handleUpdate(ctx, u); err != nil {
				b.Logger.Printf("telegram: handle update %d: %v", u.UpdateID, err)
			}
			if u.UpdateID >= offset {
				offset = u.UpdateID
			}
		}
	}
}

// handleUpdate processes a single update. Visible to tests so the
// full claim flow can be exercised without a real long-poll loop.
func (b *Bot) handleUpdate(ctx context.Context, u Update) error {
	if u.Message == nil {
		return nil
	}
	if u.Message.Chat == nil || u.Message.Chat.ID != b.GroupChatID {
		return nil
	}
	if u.Message.Text != "/start" {
		return nil
	}
	if u.Message.From == nil {
		return nil
	}
	userID := u.Message.From.ID
	username := u.Message.From.Username
	firstName := u.Message.From.FirstName

	now := time.Now().Unix()
	if err := store.UpsertTelegramUser(ctx, b.DB, store.TelegramUser{
		ID:         userID,
		Username:   username,
		FirstName:  firstName,
		LastSeenAt: now,
		IsMember:   true,
	}); err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

	priv, pub, err := b.GenKeyPair()
	if err != nil {
		b.Logger.Printf("telegram: keygen for user %d: %v", userID, err)
		_ = b.Sender.SendMessage(ctx, userID, "internal error: cannot generate keypair. try again later.")
		return nil
	}

	ip, err := ipam.Allocate(ctx, b.DB, b.CIDR, b.ServerAddr)
	if err != nil {
		b.Logger.Printf("telegram: ip alloc for user %d: %v", userID, err)
		_ = b.Sender.SendMessage(ctx, userID, "internal error: no free IPs. contact admin.")
		return nil
	}

	peerID, err := store.ClaimPeerForTelegramUser(ctx, b.DB, userID, store.Peer{
		Name:       "tg-" + strconv.FormatInt(userID, 10),
		PublicKey:  pub,
		PrivateKey: priv,
		AssignedIP: ip,
		CreatedAt:  now,
	}, b.PerUserQuota)
	if err != nil {
		if errors.Is(err, store.ErrQuotaExceeded) {
			_ = b.Sender.SendMessage(ctx, userID, fmt.Sprintf(
				"you've reached your quota of %d configs. ask the admin to raise it or revoke an old one.",
				b.PerUserQuota))
			return nil
		}
		return fmt.Errorf("claim peer: %w", err)
	}

	peer, err := store.GetPeerByID(ctx, b.DB, peerID)
	if err != nil {
		return fmt.Errorf("read back peer: %w", err)
	}

	conf := wg.GenerateClientConfig(wg.ClientConfig{
		ClientPrivateKey: peer.PrivateKey,
		ClientAddress:    peer.AssignedIP,
		DNSServers:       b.DNSServers,
		ServerPublicKey:  b.ServerPublicKey,
		ServerEndpoint:   b.ServerEndpoint,
		AllowedIPs:       "0.0.0.0/0, ::/0",
		Keepalive:        25,
	})

	filename := peer.Name + ".conf"
	if err := b.Sender.SendDocument(ctx, userID, filename, []byte(conf), "your WireGuard config"); err != nil {
		return fmt.Errorf("send document: %w", err)
	}
	b.Logger.Printf("telegram: sent %s to user %d (peer %d)", filename, userID, peerID)
	return nil
}
