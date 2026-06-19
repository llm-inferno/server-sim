package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConcurrencyFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "labels")
	os.WriteFile(p, []byte(`inferno.server.allocation.maxbatchsize="64"`+"\n"), 0o644)
	got, ok := concurrencyFromFile(p)
	if !ok || got != 64 {
		t.Fatalf("got %d ok=%v, want 64 true", got, ok)
	}

	os.WriteFile(p, []byte(`other="x"`+"\n"), 0o644)
	if _, ok := concurrencyFromFile(p); ok {
		t.Fatal("ok should be false when maxbatchsize absent")
	}
}
