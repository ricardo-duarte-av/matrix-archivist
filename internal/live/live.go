// Package live tracks what the archiver is doing right now (one slot per worker, plus
// temporary slots for manual checks) and a feed of recent events, for the live activity page.
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Hub holds the live state. All methods are safe for concurrent use; a nil *Hub and a nil
// *Slot ignore every call, so instrumented code works without one.
type Hub struct {
	mu      sync.Mutex
	slots   []*Slot
	events  []Event
	seq     int64
	cycle   Cycle
	changed chan struct{}
	nextID  int

	// Disk reports free/total bytes of the storage volume (optional).
	Disk     func() (free, total uint64, err error)
	diskAt   time.Time
	diskFree uint64
	diskTot  uint64
}

const maxEvents = 200

// Event is one line in the activity feed.
type Event struct {
	Seq    int64     `json:"seq"`
	Time   time.Time `json:"time"`
	RepoID int64     `json:"repo_id,omitempty"`
	Repo   string    `json:"repo,omitempty"`
	Kind   string    `json:"kind"` // info, change, release, prune, error, gone, recovered
	Text   string    `json:"text"`
}

// Cycle describes the scheduled check cycle.
type Cycle struct {
	Running  bool          `json:"running"`
	Done     int           `json:"done"`
	Total    int           `json:"total"`
	Started  time.Time     `json:"started"`
	LastEnd  time.Time     `json:"last_end"`
	LastTook time.Duration `json:"last_took_ms"`
	Next     time.Time     `json:"next"`
}

// New creates a hub with one permanent slot per worker.
func New(workers int) *Hub {
	h := &Hub{changed: make(chan struct{})}
	for i := range workers {
		h.slots = append(h.slots, &Slot{hub: h, ID: fmt.Sprintf("w%d", i+1), Label: fmt.Sprintf("Worker %d", i+1), Worker: true})
	}
	return h
}

// bump wakes up subscribers. Caller holds h.mu.
func (h *Hub) bump() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// Changed returns a channel that is closed at the next state change.
func (h *Hub) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changed
}

// Worker returns the permanent slot of worker i (0-based), or a temporary one if there is
// no such worker.
func (h *Hub) Worker(i int) *Slot {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.slots {
		if s.Worker {
			if n == i {
				return s
			}
			n++
		}
	}
	return h.tempLocked("Worker")
}

// Temp adds a slot for a check outside the worker pool (add, refresh, import). It is removed
// when the check ends.
func (h *Hub) Temp(label string) *Slot {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tempLocked(label)
}

func (h *Hub) tempLocked(label string) *Slot {
	h.nextID++
	s := &Slot{hub: h, ID: fmt.Sprintf("t%d", h.nextID), Label: label}
	h.slots = append(h.slots, s)
	h.bump()
	return s
}

// Event appends to the activity feed.
func (h *Hub) Event(repoID int64, repo, kind, text string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	h.events = append(h.events, Event{Seq: h.seq, Time: time.Now(), RepoID: repoID, Repo: repo, Kind: kind, Text: text})
	if len(h.events) > maxEvents {
		h.events = append([]Event(nil), h.events[len(h.events)-maxEvents:]...)
	}
	h.bump()
}

func (h *Hub) CycleStart(total int) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cycle.Running, h.cycle.Done, h.cycle.Total, h.cycle.Started = true, 0, total, time.Now()
	h.bump()
}

func (h *Hub) CycleTick() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cycle.Done++
	h.bump()
}

func (h *Hub) CycleEnd() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cycle.Running = false
	h.cycle.LastEnd = time.Now()
	h.cycle.LastTook = h.cycle.LastEnd.Sub(h.cycle.Started)
	h.bump()
}

func (h *Hub) SetNext(t time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cycle.Next = t
	h.bump()
}

// Activity returns what is being done to a repository right now ("" if nothing).
func (h *Hub) Activity(repoID int64) (step string, since time.Time) {
	if h == nil {
		return "", time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.slots {
		if s.RepoID == repoID && !s.Started.IsZero() {
			step = s.Step
			if s.Detail != "" {
				step += ": " + s.Detail
			}
			return step, s.Started
		}
	}
	return "", time.Time{}
}

// Slot is one unit of work in progress (a worker, or a manual check).
type Slot struct {
	hub    *Hub
	ID     string `json:"id"`
	Label  string `json:"label"`
	Worker bool   `json:"worker"`

	RepoID  int64     `json:"repo_id,omitempty"`
	Repo    string    `json:"repo,omitempty"`
	About   string    `json:"about,omitempty"`
	Started time.Time `json:"started"`
	Step    string    `json:"step,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Done    int64     `json:"done,omitempty"`
	Total   int64     `json:"total,omitempty"`
	Unit    string    `json:"unit,omitempty"` // "bytes" or "objects"
	Rate    float64   `json:"rate,omitempty"` // units per second

	stepStart time.Time
	lastSent  time.Time
	rateAt    time.Time
	rateDone  int64
}

// Begin marks the slot busy with a repository.
func (s *Slot) Begin(repoID int64, repo, about string) {
	if s == nil {
		return
	}
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	s.RepoID, s.Repo, s.About, s.Started = repoID, repo, about, time.Now()
	s.setStepLocked("Starting", "")
	h.bump()
}

// SetStep sets the current step and clears progress.
func (s *Slot) SetStep(step, detail string) {
	if s == nil {
		return
	}
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	s.setStepLocked(step, detail)
	h.bump()
}

func (s *Slot) setStepLocked(step, detail string) {
	s.Step, s.Detail = step, detail
	s.Done, s.Total, s.Unit, s.Rate = 0, 0, "", 0
	s.stepStart, s.rateAt, s.rateDone = time.Now(), time.Now(), 0
}

// SetDetail changes the detail line of the current step without resetting progress.
func (s *Slot) SetDetail(detail string) {
	if s == nil {
		return
	}
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.Detail != detail {
		s.Detail = detail
		h.bump()
	}
}

// Progress reports done of total (total 0 = unknown). Updates are rate-limited.
func (s *Slot) Progress(done, total int64, unit string) {
	if s == nil {
		return
	}
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if unit != s.Unit || done < s.rateDone {
		s.rateAt, s.rateDone, s.Rate = now, done, 0
	}
	s.Done, s.Total, s.Unit = done, total, unit
	if dt := now.Sub(s.rateAt).Seconds(); dt >= 1 {
		inst := float64(done-s.rateDone) / dt
		if s.Rate == 0 {
			s.Rate = inst
		} else {
			s.Rate = 0.6*s.Rate + 0.4*inst
		}
		s.rateAt, s.rateDone = now, done
	}
	if now.Sub(s.lastSent) >= 250*time.Millisecond || (total > 0 && done >= total) {
		s.lastSent = now
		h.bump()
	}
}

// End marks the slot idle (workers) or removes it (temporary slots).
func (s *Slot) End() {
	if s == nil {
		return
	}
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.Worker {
		s.RepoID, s.Repo, s.About, s.Started = 0, "", "", time.Time{}
		s.setStepLocked("", "")
	} else {
		for i, x := range h.slots {
			if x == s {
				h.slots = append(h.slots[:i:i], h.slots[i+1:]...)
				break
			}
		}
	}
	h.bump()
}

type ctxKey struct{}

// WithSlot attaches a slot to ctx so deeper code (git, downloads) can report progress.
func WithSlot(ctx context.Context, s *Slot) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// From returns the slot attached to ctx (nil if none).
func From(ctx context.Context) *Slot {
	s, _ := ctx.Value(ctxKey{}).(*Slot)
	return s
}

// Snapshot is the JSON sent to the live page.
type Snapshot struct {
	Now       time.Time `json:"now"`
	Cycle     Cycle     `json:"cycle"`
	DiskFree  uint64    `json:"disk_free,omitempty"`
	DiskTotal uint64    `json:"disk_total,omitempty"`
	Slots     []Slot    `json:"slots"`
	Events    []Event   `json:"events"`
	LastSeq   int64     `json:"last_seq"`
}

// Snapshot returns the current state and the events after sinceSeq (at most maxEvents).
func (h *Hub) Snapshot(sinceSeq int64) Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	snap := Snapshot{Now: time.Now(), Cycle: h.cycle, LastSeq: h.seq, Slots: []Slot{}, Events: []Event{}}
	for _, s := range h.slots {
		c := *s
		c.hub = nil
		snap.Slots = append(snap.Slots, c)
	}
	for _, e := range h.events {
		if e.Seq > sinceSeq {
			snap.Events = append(snap.Events, e)
		}
	}
	if h.Disk != nil && time.Since(h.diskAt) > 30*time.Second {
		if f, t, err := h.Disk(); err == nil {
			h.diskFree, h.diskTot = f, t
		}
		h.diskAt = time.Now()
	}
	snap.DiskFree, snap.DiskTotal = h.diskFree, h.diskTot
	return snap
}

// MarshalJSON encodes durations as milliseconds.
func (c Cycle) MarshalJSON() ([]byte, error) {
	type alias Cycle
	return json.Marshal(struct {
		alias
		LastTook int64 `json:"last_took_ms"`
	}{alias(c), c.LastTook.Milliseconds()})
}
