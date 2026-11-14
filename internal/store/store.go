// Package store persists the repository list, archived releases and change events in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")
var ErrDuplicate = errors.New("already exists")

type Repo struct {
	ID          int64
	URL         string
	Key         string
	RelDir      string
	AddedBy     string
	AddedAt     time.Time
	LastChecked time.Time
	LastChanged time.Time
	LastError   string
	FailCount   int
	Cloned      bool

	// Upstream is "" while the upstream repo is believed to exist, or one of UpstreamGone,
	// UpstreamLegal, UpstreamBlocked once a failing check found it missing.
	Upstream       string
	UpstreamDetail string
	UpstreamSince  time.Time

	Notes     string
	Tags      []string
	SizeBytes int64
}

const (
	UpstreamGone    = "gone"    // 404/410: deleted, renamed away or made private
	UpstreamLegal   = "legal"   // 451: unavailable for legal reasons (DMCA etc.)
	UpstreamBlocked = "blocked" // forge blocked access (ToS, etc.)
)

type Event struct {
	ID     int64
	RepoID int64
	Time   time.Time
	Kind   string // commits | release | error | added | recovered
	Detail string
}

const (
	EventUpdate    = "update"
	EventRelease   = "release"
	EventError     = "error"
	EventAdded     = "added"
	EventRecovered = "recovered"
	EventGone      = "gone"
)

type Store struct {
	db *sql.DB
}

// OpenSQLite opens a SQLite database using the pure-Go driver with WAL and foreign keys.
func OpenSQLite(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_txlock=immediate", path)
	return sql.Open("sqlite", dsn)
}

func Open(path string) (*Store, error) {
	db, err := OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE repos (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		url          TEXT NOT NULL,
		key          TEXT NOT NULL UNIQUE,
		rel_dir      TEXT NOT NULL,
		added_by     TEXT NOT NULL,
		added_at     INTEGER NOT NULL,
		last_checked INTEGER NOT NULL DEFAULT 0,
		last_changed INTEGER NOT NULL DEFAULT 0,
		last_error   TEXT NOT NULL DEFAULT '',
		fail_count   INTEGER NOT NULL DEFAULT 0,
		cloned       INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE releases (
		repo_id      INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
		tag          TEXT NOT NULL,
		name         TEXT NOT NULL,
		published_at INTEGER NOT NULL,
		archived_at  INTEGER NOT NULL,
		PRIMARY KEY (repo_id, tag)
	);
	CREATE TABLE events (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
		time    INTEGER NOT NULL,
		kind    TEXT NOT NULL,
		detail  TEXT NOT NULL
	);
	CREATE INDEX events_time ON events(time);
	CREATE TABLE forge_cache (
		host       TEXT PRIMARY KEY,
		type       TEXT NOT NULL,
		checked_at INTEGER NOT NULL
	);
	CREATE TABLE kv (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);`,
	`ALTER TABLE repos ADD COLUMN upstream TEXT NOT NULL DEFAULT '';
	ALTER TABLE repos ADD COLUMN upstream_detail TEXT NOT NULL DEFAULT '';
	ALTER TABLE repos ADD COLUMN upstream_since INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE repos ADD COLUMN notes TEXT NOT NULL DEFAULT '';
	ALTER TABLE repos ADD COLUMN tags TEXT NOT NULL DEFAULT '';
	ALTER TABLE repos ADD COLUMN size_bytes INTEGER NOT NULL DEFAULT 0;`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

const repoCols = `id, url, key, rel_dir, added_by, added_at, last_checked, last_changed, last_error, fail_count, cloned,
	upstream, upstream_detail, upstream_since, notes, tags, size_bytes`

func scanRepo(row interface{ Scan(...any) error }) (*Repo, error) {
	var r Repo
	var added, checked, changed, upSince int64
	var tags string
	if err := row.Scan(&r.ID, &r.URL, &r.Key, &r.RelDir, &r.AddedBy, &added, &checked, &changed, &r.LastError, &r.FailCount, &r.Cloned,
		&r.Upstream, &r.UpstreamDetail, &upSince, &r.Notes, &tags, &r.SizeBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.AddedAt, r.LastChecked, r.LastChanged = fromUnix(added), fromUnix(checked), fromUnix(changed)
	r.UpstreamSince = fromUnix(upSince)
	r.Tags = SplitTags(tags)
	return &r, nil
}

// SplitTags parses the stored comma-separated tag list.
func SplitTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// NormalizeTag lowercases a tag and strips characters that would break storage or display.
func NormalizeTag(t string) string {
	t = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(t, "#")))
	return strings.Map(func(r rune) rune {
		if r == ',' || r == '`' || r == '*' || r == '_' || r < 0x20 {
			return -1
		}
		if r == ' ' {
			return '-'
		}
		return r
	}, t)
}

// SetUpstream records the upstream status ("" = available). The since time is kept while
// the status doesn't change.
func (s *Store) SetUpstream(ctx context.Context, id int64, status, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET
		upstream_since = CASE WHEN ? = '' THEN 0 WHEN upstream = ? THEN upstream_since ELSE ? END,
		upstream = ?, upstream_detail = ? WHERE id = ?`,
		status, status, time.Now().Unix(), status, detail, id)
	return err
}

func (s *Store) SetNotes(ctx context.Context, id int64, notes string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET notes = ? WHERE id = ?`, notes, id)
	return err
}

// SetTags replaces a repo's tags (normalized, de-duplicated, sorted).
func (s *Store) SetTags(ctx context.Context, id int64, tags []string) error {
	seen := map[string]bool{}
	var clean []string
	for _, t := range tags {
		if t = NormalizeTag(t); t != "" && !seen[t] {
			seen[t] = true
			clean = append(clean, t)
		}
	}
	sort.Strings(clean)
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET tags = ? WHERE id = ?`, strings.Join(clean, ","), id)
	return err
}

func (s *Store) SetSize(ctx context.Context, id int64, size int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET size_bytes = ? WHERE id = ?`, size, id)
	return err
}

func (s *Store) AddRepo(ctx context.Context, url, key, relDir, addedBy string) (*Repo, error) {
	if existing, err := s.RepoByKey(ctx, key); err == nil {
		return existing, ErrDuplicate
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO repos (url, key, rel_dir, added_by, added_at) VALUES (?, ?, ?, ?, ?)`,
		url, key, relDir, addedBy, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Repo(ctx, id)
}

func (s *Store) Repo(ctx context.Context, id int64) (*Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, `SELECT `+repoCols+` FROM repos WHERE id = ?`, id))
}

func (s *Store) RepoByKey(ctx context.Context, key string) (*Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, `SELECT `+repoCols+` FROM repos WHERE key = ?`, key))
}

func (s *Store) Repos(ctx context.Context) ([]*Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoCols+` FROM repos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RelDirInUse reports whether another repo (other than exceptID) uses the same storage dir.
func (s *Store) RelDirInUse(ctx context.Context, relDir string, exceptID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repos WHERE rel_dir = ? AND id != ?`, relDir, exceptID).Scan(&n)
	return n > 0, err
}

func (s *Store) RemoveRepo(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkChecked records the outcome of a check. A nil checkErr resets the failure counter.
func (s *Store) MarkChecked(ctx context.Context, id int64, changed bool, checkErr error) error {
	now := time.Now().Unix()
	if checkErr != nil {
		_, err := s.db.ExecContext(ctx,
			`UPDATE repos SET last_checked = ?, last_error = ?, fail_count = fail_count + 1 WHERE id = ?`,
			now, checkErr.Error(), id)
		return err
	}
	q := `UPDATE repos SET last_checked = ?, last_error = '', fail_count = 0, cloned = 1 WHERE id = ?`
	args := []any{now, id}
	if changed {
		q = `UPDATE repos SET last_checked = ?, last_changed = ?, last_error = '', fail_count = 0, cloned = 1 WHERE id = ?`
		args = []any{now, now, id}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s *Store) AddEvent(ctx context.Context, repoID int64, kind, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (repo_id, time, kind, detail) VALUES (?, ?, ?, ?)`,
		repoID, time.Now().Unix(), kind, detail)
	return err
}

// EventsSince returns events with time > since, ordered by repo then time.
func (s *Store) EventsSince(ctx context.Context, since time.Time) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, time, kind, detail FROM events WHERE time > ? ORDER BY repo_id, time`, unix(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&e.ID, &e.RepoID, &t, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = fromUnix(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents deletes events older than the cutoff.
func (s *Store) PruneEvents(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE time < ?`, unix(before))
	return err
}

func (s *Store) HasRelease(ctx context.Context, repoID int64, tag string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM releases WHERE repo_id = ? AND tag = ?`, repoID, tag).Scan(&n)
	return n > 0, err
}

func (s *Store) AddRelease(ctx context.Context, repoID int64, tag, name string, published time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO releases (repo_id, tag, name, published_at, archived_at) VALUES (?, ?, ?, ?, ?)`,
		repoID, tag, name, unix(published), time.Now().Unix())
	return err
}

// ReleaseCounts returns the number of archived releases per repo.
func (s *Store) ReleaseCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id, COUNT(*) FROM releases GROUP BY repo_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// LatestRelease returns the most recently published archived release ("" tag if none).
func (s *Store) LatestRelease(ctx context.Context, repoID int64) (tag string, published time.Time, err error) {
	var ts int64
	err = s.db.QueryRowContext(ctx,
		`SELECT tag, published_at FROM releases WHERE repo_id = ? ORDER BY published_at DESC, archived_at DESC LIMIT 1`, repoID).Scan(&tag, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return tag, fromUnix(ts), err
}

func (s *Store) ReleaseCount(ctx context.Context, repoID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM releases WHERE repo_id = ?`, repoID).Scan(&n)
	return n, err
}

// ForgeType returns the cached forge type for a host ("" = none detected). ok is false if not cached
// or the cache entry is older than maxAge.
func (s *Store) ForgeType(ctx context.Context, host string, maxAge time.Duration) (typ string, ok bool) {
	var checked int64
	err := s.db.QueryRowContext(ctx, `SELECT type, checked_at FROM forge_cache WHERE host = ?`, host).Scan(&typ, &checked)
	if err != nil || time.Since(fromUnix(checked)) > maxAge {
		return "", false
	}
	return typ, true
}

func (s *Store) SetForgeType(ctx context.Context, host, typ string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO forge_cache (host, type, checked_at) VALUES (?, ?, ?)
		 ON CONFLICT(host) DO UPDATE SET type = excluded.type, checked_at = excluded.checked_at`,
		host, typ, time.Now().Unix())
	return err
}

func (s *Store) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) Set(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
