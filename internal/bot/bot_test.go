package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestAlertContent(t *testing.T) {
	c := AlertContent("**#3** x failed")
	if c.MsgType != event.MsgText || c.Mentions == nil || !c.Mentions.Room {
		t.Fatalf("not an @room m.text message: %+v", c)
	}
	if !strings.HasPrefix(c.Body, "@room ") {
		t.Errorf("body %q", c.Body)
	}
	raw, _ := json.Marshal(c)
	if !strings.Contains(string(raw), `"m.mentions":{"room":true}`) {
		t.Errorf("json %s", raw)
	}
}

func TestReplyRelation(t *testing.T) {
	plain := &event.Event{ID: "$cmd", Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "!a list"}}}
	if rel := ReplyRelation(plain); rel.GetReplyTo() != "$cmd" || rel.Type != "" {
		t.Errorf("plain reply must not start a thread: %+v", rel)
	}
	inThread := &event.Event{ID: "$cmd2", Content: event.Content{Parsed: &event.MessageEventContent{
		MsgType: event.MsgText, Body: "!a list",
		RelatesTo: (&event.RelatesTo{}).SetThread("$root", "$x"),
	}}}
	if rel := ReplyRelation(inThread); rel.GetThreadParent() != "$root" || rel.GetReplyTo() != "$cmd2" || rel.IsFallingBack {
		t.Errorf("in-thread reply: %+v", rel)
	}
}

func TestRepoTable(t *testing.T) {
	r := &store.Repo{ID: 3, URL: "https://github.com/snesrev/zelda3", Cloned: true, Tags: []string{"zelda"},
		ReadmeTitle: "Zelda3", ReadmeSummary: "A reimplementation of Zelda 3.",
		Links: []string{"discord.gg/AJJbJAzNNJ"}, Homepage: "https://github.com/snesrev/zelda3"}
	evil := &store.Repo{ID: 4, URL: "https://github.com/x/y", Description: `<img src=x onerror=alert(1)> **bold**`,
		Homepage: "javascript:alert(1)"}
	msgs := TableMessages("**Tracked (2)**", listHead, [][]cell{repoRow(r), repoRow(evil)}, "")
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	c := msgs[0].Content()
	for _, want := range []string{
		"<p><strong>Tracked (2)</strong></p><table><thead><tr><th>#</th>",
		`<td><a href="https://github.com/snesrev/zelda3">github.com/snesrev/zelda3</a> <code>zelda</code></td>`,
		"<td>A reimplementation of Zelda 3.</td>",
		`<td><a href="https://discord.gg/AJJbJAzNNJ">discord.gg/AJJbJAzNNJ</a></td>`,
		"&lt;img src=x onerror=alert(1)&gt; **bold**",
	} {
		if !strings.Contains(c.FormattedBody, want) {
			t.Errorf("HTML missing %q:\n%s", want, c.FormattedBody)
		}
	}
	if strings.Contains(c.FormattedBody, "javascript:") || strings.Contains(c.FormattedBody, "<img") {
		t.Errorf("unsafe HTML:\n%s", c.FormattedBody)
	}
	if !strings.Contains(c.Body, "#3 · github.com/snesrev/zelda3 `zelda` · A reimplementation of Zelda 3. · discord.gg/AJJbJAzNNJ · ⏳") &&
		!strings.Contains(c.Body, "#3 · github.com/snesrev/zelda3 `zelda` · A reimplementation of Zelda 3. · discord.gg/AJJbJAzNNJ · ✅") {
		t.Errorf("text body:\n%s", c.Body)
	}
	if c.Mentions == nil || c.Mentions.Room {
		t.Error("tables must not mention anyone")
	}
}

func TestTableSplit(t *testing.T) {
	var rows [][]cell
	for i := range 1000 {
		r := &store.Repo{ID: int64(i), URL: fmt.Sprintf("https://github.com/owner/repository-%d", i), Description: strings.Repeat("words ", 25)}
		rows = append(rows, repoRow(r))
	}
	msgs := TableMessages("intro", listHead, rows, "outro")
	if len(msgs) < 5 {
		t.Fatalf("expected a split, got %d messages", len(msgs))
	}
	n := 0
	for i, m := range msgs {
		c := m.Content()
		if len(c.Body)+len(c.FormattedBody) > maxRichBytes+2048 {
			t.Errorf("message %d too big: %d", i, len(c.Body)+len(c.FormattedBody))
		}
		n += strings.Count(c.FormattedBody, "<tr><td>")
		if (i == 0) != strings.Contains(c.Body, "intro") || (i == len(msgs)-1) != strings.Contains(c.Body, "outro") {
			t.Errorf("intro/outro placement in message %d", i)
		}
	}
	if n != 1000 {
		t.Errorf("%d rows sent, want 1000", n)
	}
}

func TestAboutAndRank(t *testing.T) {
	r := &store.Repo{URL: "https://github.com/a/zelda3", ReadmeTitle: "Zelda3"}
	if About(r) != "" {
		t.Errorf("title equal to repo name is not a description: %q", About(r))
	}
	r.ReadmeSummary = "A reimplementation."
	if About(r) != "A reimplementation." {
		t.Error(About(r))
	}
	r.Description = "From the forge"
	if About(r) != "From the forge" {
		t.Error(About(r))
	}
	repos := []*store.Repo{
		{ID: 1, URL: "https://github.com/x/readme-only"},
		{ID: 2, URL: "https://github.com/x/other", Description: "Mario recompilation"},
		{ID: 3, URL: "https://github.com/x/mario-recomp"},
	}
	got := RankRepos(repos, []string{"mario"})
	if got[0].ID != 3 || got[1].ID != 2 || got[2].ID != 1 {
		t.Errorf("rank: %d %d %d", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestFilterRepos(t *testing.T) {
	repos := []*store.Repo{
		{ID: 1, URL: "https://github.com/a/ytdl", Tags: []string{"video"}, Notes: "Took down in 2020"},
		{ID: 2, URL: "https://codeberg.org/b/tool", Tags: []string{"cli", "video"}},
		{ID: 3, URL: "https://gitlab.com/c/other"},
	}
	ids := func(rs []*store.Repo) (out []int64) {
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return
	}
	for _, tc := range []struct {
		terms []string
		want  string
	}{
		{[]string{"#video"}, "[1 2]"},
		{[]string{"#VIDEO", "codeberg"}, "[2]"},
		{[]string{"took"}, "[1]"},
		{[]string{"#vid"}, "[]"},
		{[]string{"gitlab"}, "[3]"},
	} {
		if got := fmt.Sprint(ids(FilterRepos(repos, tc.terms))); got != tc.want && !(tc.want == "[]" && got == "[]") {
			t.Errorf("%v: got %s want %s", tc.terms, got, tc.want)
		}
	}
}

func TestExportImport(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	data := Export([]*store.Repo{{URL: "https://github.com/a/b", Notes: "why", Tags: []string{"x"}, AddedBy: "@u:s", AddedAt: now}}, now)
	entries, err := ParseImport(data)
	if err != nil || len(entries) != 1 || entries[0].URL != "https://github.com/a/b" || entries[0].Notes != "why" || entries[0].Tags[0] != "x" {
		t.Fatalf("roundtrip: %+v %v\n%s", entries, err, data)
	}
	entries, err = ParseImport([]byte("# my list\nhttps://github.com/a/b\n\n  git@codeberg.org:c/d.git  trailing comment\n"))
	if err != nil || len(entries) != 2 || entries[1].URL != "git@codeberg.org:c/d.git" {
		t.Fatalf("text import: %+v %v", entries, err)
	}
	if _, err := ParseImport([]byte(`{"format":"something-else"}`)); err == nil {
		t.Error("unknown format accepted")
	}
	if _, err := ParseImport([]byte("# nothing\n")); err == nil {
		t.Error("empty import accepted")
	}
}

func TestWidgetContent(t *testing.T) {
	c := WidgetContent(activityWidgetID, "https://archive.example.org/live/", "Archive activity", "t", "@archivist:example.org")
	if c["url"] != "https://archive.example.org/live/" || c["type"] != "m.custom" || c["id"] != activityWidgetID {
		t.Errorf("widget: %v", c)
	}
}

func TestLayout(t *testing.T) {
	order := []string{widgetID, activityWidgetID}
	other := map[string]any{"container": "right"}
	cur := map[string]any{"widgets": map[string]any{"someone-elses": other}}

	l, changed := Layout(cur, map[string]bool{widgetID: true, activityWidgetID: true}, order)
	ws := l["widgets"].(map[string]any)
	if !changed || ws["someone-elses"] == nil || ws[widgetID].(map[string]any)["width"] != 50 || ws[activityWidgetID].(map[string]any)["index"] != 1 {
		t.Fatalf("both pinned: %v", l)
	}
	if _, changed := Layout(l, map[string]bool{widgetID: true, activityWidgetID: true}, order); changed {
		t.Error("no-op layout reported a change")
	}
	l, changed = Layout(l, map[string]bool{widgetID: true}, order)
	ws = l["widgets"].(map[string]any)
	if !changed || ws[activityWidgetID] != nil || ws[widgetID].(map[string]any)["width"] != 100 {
		t.Fatalf("unpin one: %v", l)
	}
	if _, changed := Layout(nil, map[string]bool{}, order); changed {
		t.Error("nothing pinned, nothing to change")
	}
}
