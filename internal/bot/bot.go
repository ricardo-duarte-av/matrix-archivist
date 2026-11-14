// Package bot connects to Matrix and dispatches room commands.
package bot

import (
	"context"
	"fmt"
	"math"
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
	syncer.OnSync(b.onSyncInvites)
	// Note: no DontProcessOldEvents. It drops the whole room from the sync in which the bot
	// joins, so the room state (m.room.create, power levels, encryption) would never reach the
	// state store. Old messages are filtered by timestamp in onMessage instead.
	syncer.OnEventType(event.EventMessage, b.onMessage)

	b.ensureJoined(ctx)
	if b.cfg.Matrix.DisplayName != "" {
		_ = b.client.SetDisplayName(ctx, b.cfg.Matrix.DisplayName)
	}

	b.arch.Notify = b.Notify
	b.arch.Alert = b.Alert
	err = b.client.SyncWithContext(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// ensureJoined joins the room at startup if the bot isn't in it yet. A pending invite may have
// been delivered (and failed) in an earlier run, after which the stored sync token skips it, so
// do one fresh sync to find it and learn which servers are in the room.
func (b *Bot) ensureJoined(ctx context.Context) {
	if joined, err := b.client.JoinedRooms(ctx); err == nil {
		for _, r := range joined.JoinedRooms {
			if r == b.roomID {
				b.log.Info().Msg("Already in room")
				b.refreshState(ctx)
				return
			}
		}
	}
	var stripped []*event.Event
	if resp, err := b.client.FullSyncRequest(ctx, mautrix.ReqSync{Timeout: 0}); err != nil {
		b.log.Warn().Err(err).Msg("Initial sync for pending invites failed")
	} else {
		if inv, ok := resp.Rooms.Invite[b.roomID]; ok {
			stripped = inv.State.Events
			b.log.Info().Msg("Found pending invite to room")
		}
		for roomID := range resp.Rooms.Invite {
			if roomID != b.roomID {
				b.log.Warn().Str("invited_room", fmt.Sprintf("%q", roomID)).Str("configured_room", fmt.Sprintf("%q", b.roomID)).
					Msg("Pending invite to a room that is not matrix.room_id")
			}
		}
	}
	if err := b.join(ctx, stripped); err != nil {
		b.log.Warn().Err(err).Msg("Could not join room yet (invite the bot; it will join automatically)")
	}
}

// onSyncInvites joins the configured room when an invite to it arrives.
func (b *Bot) onSyncInvites(ctx context.Context, resp *mautrix.RespSync, _ string) bool {
	for roomID, inv := range resp.Rooms.Invite {
		if roomID != b.roomID {
			b.log.Warn().
				Str("invited_room", fmt.Sprintf("%q", roomID)).
				Str("configured_room", fmt.Sprintf("%q", b.roomID)).
				Strs("servers", ServersFromInviteState(inv.State.Events, b.client.UserID)).
				Msg("Invited to a room that is not matrix.room_id; ignoring. If this is the right room, fix room_id in config.yaml")
		}
	}
	if inv, ok := resp.Rooms.Invite[b.roomID]; ok {
		b.log.Info().Msg("Invited to room, joining")
		go func() {
			if err := b.join(b.ctx, inv.State.Events); err != nil {
				b.log.Err(err).Msg("Failed to join room")
			}
		}()
	}
	return true
}

// join joins the configured room. Room v12+ IDs carry no server name, so the homeserver needs
// explicit "via" servers that are already in the room. They are taken from the configured
// room_via, the invite's stripped state (inviter, room creator, joined members), the room ID's
// server if present, and the bot's own server.
func (b *Bot) join(ctx context.Context, inviteState []*event.Event) error {
	extra := append([]string{}, b.cfg.Matrix.RoomVia...)
	extra = append(extra, ServersFromInviteState(inviteState, b.client.UserID)...)
	via := JoinVia(b.roomID, b.client.UserID, extra...)
	_, err := b.client.JoinRoom(ctx, b.roomID.String(), &mautrix.ReqJoinRoom{Via: via})
	if err != nil {
		return fmt.Errorf("join via %v: %w", via, err)
	}
	b.log.Info().Strs("via", via).Msg("Joined room")
	b.refreshState(ctx)
	return nil
}

// ServersFromInviteState extracts candidate servers from an invite's stripped state, most
// useful first: the inviter, the room creator, then servers of joined members and other senders.
func ServersFromInviteState(evts []*event.Event, self id.UserID) []string {
	var inviter, creator []string
	var others []string
	for _, e := range evts {
		switch {
		case e.Type == event.StateMember && e.GetStateKey() == self.String():
			inviter = append(inviter, e.Sender.Homeserver())
		case e.Type == event.StateCreate:
			creator = append(creator, e.Sender.Homeserver())
		case e.Type == event.StateMember:
			if m, _ := e.Content.Raw["membership"].(string); m == "join" {
				others = append(others, id.UserID(e.GetStateKey()).Homeserver())
			}
		default:
			others = append(others, e.Sender.Homeserver())
		}
	}
	return append(append(inviter, creator...), others...)
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

// Alert posts an @room message that notifies everyone in the room. It is sent as m.text,
// because the default push rules suppress notifications for m.notice.
func (b *Bot) Alert(ctx context.Context, markdown string) {
	if ctx.Err() != nil {
		ctx = b.ctx
	}
	c := AlertContent(markdown)
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, c); err != nil {
		b.log.Err(err).Msg("Failed to send alert")
	}
}

// AlertContent builds an @room message: the body starts with "@room" (for clients and push
// rules that match on the body) and m.mentions.room is set (intentional mentions).
func AlertContent(markdown string) *event.MessageEventContent {
	c := format.RenderMarkdown("@room "+markdown, true, false)
	c.MsgType = event.MsgText
	c.Mentions = &event.Mentions{Room: true}
	return &c
}

// refreshState fetches the full room state into the state store (encryption, power levels,
// create event), so it is correct even if earlier syncs were missed.
func (b *Bot) refreshState(ctx context.Context) {
	state, err := b.client.State(ctx, b.roomID)
	if err != nil {
		b.log.Warn().Err(err).Msg("Failed to fetch room state")
		return
	}
	if b.cfg.FailureAlert > 0 {
		plEvt := state[event.StatePowerLevels][""]
		need := 50
		if plEvt != nil {
			need = plEvt.Content.AsPowerLevels().Notifications.Room()
		}
		if have := UserLevel(plEvt, state[event.StateCreate][""], b.client.UserID); have < need {
			b.log.Warn().Int("bot_level", have).Int("needed", need).
				Msg("Bot's power level is too low for @room alerts to notify anyone; raise it in the room settings")
		}
	}
	b.syncWidget(ctx)
}

// powerLevel returns the user's power level, fetched fresh from the server. Room creators in
// room v12+ (create event sender and additional_creators) get math.MaxInt.
func (b *Bot) powerLevel(ctx context.Context, user id.UserID) int {
	state, err := b.client.State(ctx, b.roomID)
	if err != nil {
		b.log.Warn().Err(err).Msg("Failed to fetch room state; using cached power levels")
		pl, err := b.client.StateStore.GetPowerLevels(ctx, b.roomID)
		if err != nil || pl == nil {
			return 0
		}
		return pl.GetUserLevel(user)
	}
	return UserLevel(state[event.StatePowerLevels][""], state[event.StateCreate][""], user)
}

// UserLevel computes a user's power level from the power levels and create events (either
// may be nil). Creators are unlimited in room v12+; without a power levels event the creator
// has 100 in older rooms, as per the spec.
func UserLevel(plEvt, createEvt *event.Event, user id.UserID) int {
	if plEvt != nil {
		pl := plEvt.Content.AsPowerLevels()
		pl.CreateEvent = createEvt
		return pl.GetUserLevel(user)
	}
	if createEvt != nil {
		if (&event.PowerLevelsEventContent{CreateEvent: createEvt}).GetUserLevel(user) == math.MaxInt {
			return math.MaxInt
		}
		if createEvt.Sender == user {
			return 100
		}
	}
	return 0
}
