package live

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHub(t *testing.T) {
	h := New(2)
	ch := h.Changed()
	w := h.Worker(0)
	w.Begin(7, "https://github.com/o/r", "About")
	select {
	case <-ch:
	default:
		t.Fatal("Begin did not signal a change")
	}
	w.SetStep("Downloading release", "v1 (1/2)")
	w.Progress(50, 100, "bytes")
	if step, since := h.Activity(7); step != "Downloading release: v1 (1/2)" || since.IsZero() {
		t.Errorf("activity %q", step)
	}
	tmp := h.Temp("Manual check")
	tmp.Begin(8, "https://x.org/a/b", "")
	h.Event(7, "https://github.com/o/r", "change", "2 new commits")
	h.CycleStart(10)
	h.CycleTick()

	snap := h.Snapshot(0)
	if len(snap.Slots) != 3 || snap.Slots[0].Done != 50 || snap.Slots[2].Label != "Manual check" || len(snap.Events) != 1 || snap.Cycle.Done != 1 {
		t.Fatalf("snapshot: %+v", snap)
	}
	if s := h.Snapshot(snap.LastSeq); len(s.Events) != 0 {
		t.Error("events after last seq were resent")
	}
	tmp.End()
	w.End()
	snap = h.Snapshot(0)
	if len(snap.Slots) != 2 || !snap.Slots[0].Started.IsZero() || snap.Slots[0].Step != "" {
		t.Errorf("after End: %+v", snap.Slots)
	}
	b, _ := json.Marshal(snap)
	if !strings.Contains(string(b), `"last_took_ms":0`) || strings.Contains(string(b), `"hub"`) {
		t.Errorf("json: %s", b)
	}
	for range maxEvents + 10 {
		h.Event(0, "", "info", "x")
	}
	if n := len(h.Snapshot(0).Events); n != maxEvents {
		t.Errorf("feed not capped: %d", n)
	}
}

func TestNilSafe(t *testing.T) {
	var h *Hub
	s := h.Temp("x")
	s.Begin(1, "u", "")
	s.SetStep("a", "b")
	s.Progress(1, 2, "bytes")
	s.End()
	h.Event(1, "", "info", "x")
	if From(context.Background()) != nil {
		t.Error("From on empty ctx")
	}
	if From(WithSlot(context.Background(), s)) != s {
		t.Error("WithSlot/From")
	}
	_ = time.Now
}
