package bot

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/crypto"
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
