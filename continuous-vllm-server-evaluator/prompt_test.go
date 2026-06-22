package main

import (
	"testing"
)

func TestSyntheticPromptTokens_LengthAndRange(t *testing.T) {
	tokens := syntheticPromptTokens(128, 42)
	if len(tokens) != 128 {
		t.Fatalf("len = %d, want 128", len(tokens))
	}
	for _, tok := range tokens {
		if tok < syntheticTokenMin || tok > syntheticTokenMax {
			t.Errorf("token %d out of range [%d,%d]", tok, syntheticTokenMin, syntheticTokenMax)
		}
	}
}

func TestSyntheticPromptTokens_VariesAcrossSeeds(t *testing.T) {
	a := syntheticPromptTokens(64, 1)
	b := syntheticPromptTokens(64, 2)
	identical := true
	for i := range a {
		if a[i] != b[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Error("expected different tokens for different seeds (avoids prefix-cache hits)")
	}
}

func TestSyntheticPromptTokens_ZeroLength(t *testing.T) {
	if len(syntheticPromptTokens(0, 1)) != 0 {
		t.Error("zero length input should produce empty slice")
	}
}
