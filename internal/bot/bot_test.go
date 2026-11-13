package bot

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		body string
		want []string
		ok   bool
	}{
		{"!archivist add https://x.y/a/b", []string{"add", "https://x.y/a/b"}, true},
		{"  !Archivist   LIST ", []string{"list"}, true},
		{"!archivist", []string{"help"}, true},
		{"!archivistx list", nil, false},
		{"hello !archivist list", nil, false},
	}
	for _, c := range cases {
		got, ok := ParseCommand(c.body, "!archivist")
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v %v", c.body, got, ok)
		}
	}
}

func TestStripReplyFallback(t *testing.T) {
	in := "> <@a:b> original\n> more\n\n!archivist list"
	if got := stripReplyFallback(in); got != "!archivist list" {
		t.Errorf("got %q", got)
	}
}

// The crypto and state stores must work on the pure-Go SQLite driver (no CGO).
func TestCryptoStoreOnModernc(t *testing.T) {
	ctx := context.Background()
	raw, err := store.OpenSQLite(filepath.Join(t.TempDir(), "crypto.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := dbutil.NewWithDB(raw, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	ss := sqlstatestore.NewSQLStateStore(db, dbutil.NoopLogger, false)
	if err := ss.Upgrade(ctx); err != nil {
		t.Fatalf("state store upgrade: %v", err)
	}
	cs := crypto.NewSQLCryptoStore(db, dbutil.NoopLogger, "", id.DeviceID("DEV"), []byte("pickle"))
	if err := cs.DB.Upgrade(ctx); err != nil {
		t.Fatalf("crypto store upgrade: %v", err)
	}
	if _, err := cs.FindDeviceID(ctx); err != nil {
		t.Fatalf("find device: %v", err)
	}
}

func TestJoinVia(t *testing.T) {
	got := JoinVia("!k4D-BOtxgq4GW6J88ktGmlUZnNBcKRRTPXYF9v-tpDg", "@archivist:aguiarvieira.pt", "aguiarvieira.pt", "other.org")
	if !reflect.DeepEqual(got, []string{"aguiarvieira.pt", "other.org"}) {
		t.Errorf("v12 room: %v", got)
	}
	got = JoinVia("!abc:matrix.org", "@bot:example.org")
	if !reflect.DeepEqual(got, []string{"matrix.org", "example.org"}) {
		t.Errorf("legacy room: %v", got)
	}
}

func TestServersFromInviteState(t *testing.T) {
	var evts []*event.Event
	raw := `[
		{"type":"m.room.create","sender":"@creator:origin.org","state_key":"","content":{}},
		{"type":"m.room.member","sender":"@alice:inviter.org","state_key":"@archivist:aguiarvieira.pt","content":{"membership":"invite"}},
		{"type":"m.room.member","sender":"@bob:member.org","state_key":"@bob:member.org","content":{"membership":"join"}},
		{"type":"m.room.name","sender":"@carol:named.org","state_key":"","content":{"name":"x"}}
	]`
	if err := json.Unmarshal([]byte(raw), &evts); err != nil {
		t.Fatal(err)
	}
	for _, e := range evts {
		_ = e.Content.ParseRaw(e.Type)
	}
	got := ServersFromInviteState(evts, "@archivist:aguiarvieira.pt")
	want := []string{"inviter.org", "origin.org", "member.org", "named.org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func parseState(t *testing.T, raw string) *event.Event {
	t.Helper()
	var e event.Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	e.Type.Class = event.StateEventType
	if err := e.Content.ParseRaw(e.Type); err != nil {
		t.Fatal(err)
	}
	return &e
}

func TestUserLevelCreators(t *testing.T) {
	pl := parseState(t, `{"type":"m.room.power_levels","state_key":"","sender":"@daedric:aguiarvieira.pt",
		"content":{"users":{"@mod:x.org":50},"users_default":0}}`)
	v12 := parseState(t, `{"type":"m.room.create","state_key":"","sender":"@daedric:aguiarvieira.pt",
		"content":{"room_version":"12","additional_creators":["@co:x.org"]}}`)
	v11 := parseState(t, `{"type":"m.room.create","state_key":"","sender":"@daedric:aguiarvieira.pt",
		"content":{"room_version":"11"}}`)

	cases := []struct {
		name   string
		pl, cr *event.Event
		user   id.UserID
		want   int
	}{
		{"v12 creator", pl, v12, "@daedric:aguiarvieira.pt", math.MaxInt},
		{"v12 additional creator", pl, v12, "@co:x.org", math.MaxInt},
		{"v12 listed user", pl, v12, "@mod:x.org", 50},
		{"v12 other", pl, v12, "@nobody:x.org", 0},
		{"v11 creator uses users map", pl, v11, "@daedric:aguiarvieira.pt", 0},
		{"v12 no PL event", nil, v12, "@daedric:aguiarvieira.pt", math.MaxInt},
		{"v11 no PL event", nil, v11, "@daedric:aguiarvieira.pt", 100},
		{"nothing", nil, nil, "@daedric:aguiarvieira.pt", 0},
	}
	for _, c := range cases {
		if got := UserLevel(c.pl, c.cr, c.user); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
