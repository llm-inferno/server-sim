package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDownwardLabel_Present(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pair-id"), []byte("uuid-abc-123"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readDownwardLabel(dir, "pair-id")
	if err != nil {
		t.Fatalf("readDownwardLabel: %v", err)
	}
	if got != "uuid-abc-123" {
		t.Errorf("got %q, want %q", got, "uuid-abc-123")
	}
}

func TestReadDownwardLabel_StripsWhitespace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pair-id"), []byte("uuid-abc-123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := readDownwardLabel(dir, "pair-id")
	if got != "uuid-abc-123" {
		t.Errorf("got %q, want %q (newline should be stripped)", got, "uuid-abc-123")
	}
}

func TestReadDownwardLabel_Missing(t *testing.T) {
	dir := t.TempDir()
	if _, err := readDownwardLabel(dir, "pair-id"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
