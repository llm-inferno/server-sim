package config

import (
	"testing"
	"time"
)

func TestLoadContinuousDefaults(t *testing.T) {
	t.Setenv("SERVERSIM_CONTINUOUS", "true")
	cfg := Load()
	if !cfg.ContinuousMode {
		t.Fatal("ContinuousMode should be true")
	}
	if cfg.TickInterval != 5*time.Second {
		t.Fatalf("TickInterval = %v, want 5s", cfg.TickInterval)
	}
	if cfg.SaturationPolicy != SaturationPolicyRetry {
		t.Fatalf("SaturationPolicy = %q, want %q", cfg.SaturationPolicy, SaturationPolicyRetry)
	}
	if cfg.LabelsDir != "/etc/podinfo" {
		t.Fatalf("LabelsDir = %q", cfg.LabelsDir)
	}
}

func TestLoadTickFloorAndPolicyOverride(t *testing.T) {
	t.Setenv("SERVERSIM_TICK_SECONDS", "0")
	t.Setenv("SERVERSIM_SATURATION_POLICY", "pass-through")
	cfg := Load()
	if cfg.TickInterval != 1*time.Second {
		t.Fatalf("TickInterval = %v, want 1s floor", cfg.TickInterval)
	}
	if cfg.SaturationPolicy != SaturationPolicyPassThrough {
		t.Fatalf("SaturationPolicy = %q", cfg.SaturationPolicy)
	}
}
