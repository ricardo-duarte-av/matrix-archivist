// Package bot connects to Matrix and dispatches room commands.
package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/archiver"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

type Bot struct {
	cfg    *config.Config
	client *mautrix.Client
	crypto *cryptohelper.CryptoHelper
	arch   *archiver.Archiver
	store  *store.Store
	log    zerolog.Logger
	roomID id.RoomID
	start  time.Time

	// ctx lives as long as the bot; used for background work started by commands.
	ctx context.Context
}

func New(cfg *config.Config, st *store.Store, arch *archiver.Archiver, log zerolog.Logger) (*Bot, error) {
	client, err := mautrix.NewClient(cfg.Matrix.Homeserver, id.UserID(cfg.Matrix.UserID), cfg.Matrix.AccessToken)
	if err != nil {
		return nil, err
	}
	client.Log = log.With().Str("component", "matrix").Logger()
	if cfg.Matrix.AccessToken != "" {
		client.DeviceID = id.DeviceID(cfg.Matrix.DeviceID)
	}

	rawDB, err := store.OpenSQLite(cfg.CryptoDatabase)
	if err != nil {
		return nil, fmt.Errorf("open crypto db: %w", err)
	}
	cryptoDB, err := dbutil.NewWithDB(rawDB, "sqlite")
	if err != nil {
		return nil, err
	}
	helper, err := cryptohelper.NewCryptoHelper(client, []byte(cfg.Matrix.PickleKey), cryptoDB)
	if err != nil {
		return nil, fmt.Errorf("crypto helper: %w", err)
	}
	if cfg.Matrix.AccessToken == "" {
		helper.LoginAs = &mautrix.ReqLogin{
			Type:                     mautrix.AuthTypePassword,
			Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: cfg.Matrix.UserID},
			Password:                 cfg.Matrix.Password,
			InitialDeviceDisplayName: cfg.Matrix.DisplayName,
		}
	}
	helper.DecryptErrorCallback = func(evt *event.Event, err error) {
		log.Warn().Err(err).Stringer("event_id", evt.ID).Stringer("sender", evt.Sender).Msg("Failed to decrypt event")
	}

	return &Bot{
		cfg:    cfg,
		client: client,
		crypto: helper,
		arch:   arch,
		store:  st,
		log:    log,
		roomID: id.RoomID(cfg.Matrix.RoomID),
	}, nil
}

// Run initializes crypto, joins the room and syncs until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	b.ctx = ctx
	b.start = time.Now()

	if err := b.crypto.Init(ctx); err != nil {
		return fmt.Errorf("crypto init: %w", err)
	}
	b.client.Crypto = b.crypto
	defer b.crypto.Close()

	whoami, err := b.client.Whoami(ctx)
	if err != nil {
		return fmt.Errorf("whoami: %w", err)
	}
	b.log.Info().Stringer("user_id", whoami.UserID).Stringer("device_id", b.client.DeviceID).Msg("Logged in")

	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnSync(b.client.DontProcessOldEvents)
	syncer.OnEventType(event.StateMember, b.onMember)
	syncer.OnEventType(event.EventMessage, b.onMessage)

	if err := b.join(ctx); err != nil {
		b.log.Warn().Err(err).Msg("Could not join room yet (invite the bot; it will join automatically)")
	}
	if b.cfg.Matrix.DisplayName != "" {
		_ = b.client.SetDisplayName(ctx, b.cfg.Matrix.DisplayName)
	}

	b.arch.Notify = b.Notify
	err = b.client.SyncWithContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (b *Bot) onMember(ctx context.Context, evt *event.Event) {
	if evt.RoomID != b.roomID || evt.GetStateKey() != b.client.UserID.String() {
		return
	}
	if evt.Content.AsMember().Membership == event.MembershipInvite {
		b.log.Info().Stringer("inviter", evt.Sender).Msg("Invited to room, joining")
		if err := b.join(ctx, evt.Sender.Homeserver()); err != nil {
			b.log.Err(err).Msg("Failed to join room")
		}
	}
}

// join joins the configured room. Room v12+ IDs carry no server name, so the homeserver needs
// explicit "via" servers to join through: the given ones (e.g. the inviter's), the server from
// the room ID if present, and the bot's own server.
func (b *Bot) join(ctx context.Context, extraVia ...string) error {
	via := JoinVia(b.roomID, b.client.UserID, extraVia...)
	_, err := b.client.JoinRoom(ctx, b.roomID.String(), &mautrix.ReqJoinRoom{Via: via})
	if err == nil {
		b.log.Info().Strs("via", via).Msg("Joined room")
	}
	return err
}

// JoinVia builds a de-duplicated list of servers to join a room through.
func JoinVia(roomID id.RoomID, self id.UserID, extra ...string) []string {
	var via []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			via = append(via, s)
		}
	}
	for _, s := range extra {
		add(s)
	}
	if _, server, ok := strings.Cut(roomID.String(), ":"); ok {
		add(server)
	}
	add(self.Homeserver())
	return via
}

func (b *Bot) onMessage(ctx context.Context, evt *event.Event) {
	if evt.RoomID != b.roomID || evt.Sender == b.client.UserID {
		return
	}
	if time.UnixMilli(evt.Timestamp).Before(b.start.Add(-30 * time.Second)) {
		return
	}
	msg := evt.Content.AsMessage()
	if msg == nil || msg.MsgType != event.MsgText || msg.RelatesTo.GetReplaceID() != "" {
		return
	}
	body := stripReplyFallback(msg.Body)
	args, ok := ParseCommand(body, b.cfg.CommandPrefix)
	if !ok {
		return
	}
	b.log.Info().Stringer("sender", evt.Sender).Strs("args", args).Msg("Command")
	go b.handle(evt, args)
}

func stripReplyFallback(body string) string {
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], "> ") {
		i++
	}
	if i > 0 && i < len(lines) && lines[i] == "" {
		i++
	}
	return strings.Join(lines[i:], "\n")
}

// ParseCommand splits "!prefix cmd arg1 arg2" into [cmd, arg1, arg2]. A bare prefix yields ["help"].
func ParseCommand(body, prefix string) ([]string, bool) {
	fields := strings.Fields(strings.TrimSpace(body))
	if len(fields) == 0 || !strings.EqualFold(fields[0], prefix) {
		return nil, false
	}
	if len(fields) == 1 {
		return []string{"help"}, true
	}
	fields[1] = strings.ToLower(fields[1])
	return fields[1:], true
}

func (b *Bot) content(markdown string) *event.MessageEventContent {
	c := format.RenderMarkdown(markdown, true, false)
	c.MsgType = event.MsgNotice
	return &c
}

// Notify posts a message to the room (used for alerts and summaries).
func (b *Bot) Notify(ctx context.Context, markdown string) {
	if ctx.Err() != nil {
		ctx = b.ctx
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, b.content(markdown)); err != nil {
		b.log.Err(err).Msg("Failed to send message")
	}
}

// reply sends a message in reply to evt.
func (b *Bot) reply(evt *event.Event, markdown string) {
	c := b.content(markdown)
	c.RelatesTo = (&event.RelatesTo{}).SetReplyTo(evt.ID)
	if _, err := b.client.SendMessageEvent(b.ctx, b.roomID, event.EventMessage, c); err != nil {
		b.log.Err(err).Msg("Failed to send reply")
	}
}

// react adds an emoji reaction to evt (best effort).
func (b *Bot) react(evt *event.Event, key string) {
	_, _ = b.client.SendReaction(b.ctx, b.roomID, evt.ID, key)
}

func (b *Bot) powerLevel(ctx context.Context, user id.UserID) int {
	pl, err := b.client.StateStore.GetPowerLevels(ctx, b.roomID)
	if err != nil || pl == nil {
		pl = &event.PowerLevelsEventContent{}
		if err := b.client.StateEvent(ctx, b.roomID, event.StatePowerLevels, "", pl); err != nil {
			b.log.Warn().Err(err).Msg("Failed to fetch power levels")
			return 0
		}
	}
	return pl.GetUserLevel(user)
}
