package main

import "testing"

func TestResolveVLLMPort_IgnoresOmittedPorts(t *testing.T) {
	// One entry omits vllmPort (0), another sets 8000. This must resolve to 8000
	// regardless of (nondeterministic) map iteration order — an omitted port is
	// not a real value to compare against, so it must never trigger a "mismatch".
	lookup := map[string]serverConfig{
		"H100|a": {VLLMPort: 0},
		"H100|b": {VLLMPort: 8000},
	}
	for i := 0; i < 50; i++ {
		got, err := resolveVLLMPort(lookup)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 8000 {
			t.Fatalf("got port %d, want 8000", got)
		}
	}
}

func TestResolveVLLMPort_DefaultsWhenAllOmitted(t *testing.T) {
	lookup := map[string]serverConfig{"H100|a": {VLLMPort: 0}}
	got, err := resolveVLLMPort(lookup)
	if err != nil || got != 8000 {
		t.Fatalf("got (%d, %v), want (8000, nil)", got, err)
	}
}

func TestResolveVLLMPort_RejectsConflicting(t *testing.T) {
	lookup := map[string]serverConfig{
		"H100|a": {VLLMPort: 8000},
		"H100|b": {VLLMPort: 8001},
	}
	if _, err := resolveVLLMPort(lookup); err == nil {
		t.Fatal("expected error for conflicting non-zero vllmPort values")
	}
}
