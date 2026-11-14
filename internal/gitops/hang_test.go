package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A git whose helper process (like git-remote-https) outlives it while holding stdout open
// must not hang the caller past the timeout.
func TestTimeoutKillsChildren(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "git")
	os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30\necho done\n"), 0o755)
	g := &Git{Binary: fake}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := g.run(ctx, "", "fetch")
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("run returned after %v", took)
	}
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}
