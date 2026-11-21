package archiver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDatabase(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "data", "archivist.db")
	storage := filepath.Join(tmp, "repos")
	if err := CheckDatabase(db, storage); err != nil {
		t.Fatalf("first start, no storage: %v", err)
	}
	os.MkdirAll(storage, 0o755)
	if err := CheckDatabase(db, storage); err != nil {
		t.Fatalf("first start, empty storage: %v", err)
	}
	os.MkdirAll(filepath.Join(storage, "github.com", "o", "r", "mirror.git"), 0o755)
	if err := CheckDatabase(db, storage); err == nil || !strings.Contains(err.Error(), "github.com") {
		t.Fatalf("missing database over existing storage: %v", err)
	}
	os.MkdirAll(filepath.Dir(db), 0o755)
	os.WriteFile(db, nil, 0o644)
	if err := CheckDatabase(db, storage); err != nil {
		t.Fatalf("existing (empty) database: %v", err)
	}
}
