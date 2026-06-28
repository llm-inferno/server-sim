package main

import "testing"

// validBase returns a minimal modelEntry that passes validateEntry's required-field
// checks, so token-dist cases test only the tokenDist rules.
func validBase() modelEntry {
	return modelEntry{
		Accelerator:        "H100",
		Model:              "test/model",
		HFConfigPath:       "hf-configs/test/config.json",
		GPU:                "H100",
		TotalKVBlocks:      1000,
		MaxRunningReqs:     256,
		MaxScheduledTokens: 8192,
	}
}

func TestValidateEntry_TokenDist(t *testing.T) {
	cases := []struct {
		name        string
		maxModelLen int64
		td          *tokenDistConfig
		wantErr     bool
	}{
		{"nil block ok", 4096, nil, false},
		{"constant ok bounded", 4096, &tokenDistConfig{Type: "constant"}, false},
		{"gaussian ok bounded", 4096, &tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: 1}, false},
		{"lognormal ok bounded", 4096, &tokenDistConfig{Type: "lognormal", Cov: 0.5}, false},
		{"exponential ok unbounded", 0, &tokenDistConfig{Type: "exponential"}, false},
		{"exponential rejected bounded", 4096, &tokenDistConfig{Type: "exponential"}, true},
		{"unknown type rejected", 0, &tokenDistConfig{Type: "weibull"}, true},
		{"gaussian cov<=0 rejected", 4096, &tokenDistConfig{Type: "gaussian", Cov: 0}, true},
		{"lognormal cov<=0 rejected", 4096, &tokenDistConfig{Type: "lognormal", Cov: -1}, true},
		{"negative min rejected", 4096, &tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: -2}, true},
		{"fractional min rejected", 4096, &tokenDistConfig{Type: "gaussian", Cov: 0.5, Min: 0.5}, true},
		{"gaussian unbounded rejected", 0, &tokenDistConfig{Type: "gaussian", Cov: 0.5}, true},
		{"lognormal unbounded rejected", 0, &tokenDistConfig{Type: "lognormal", Cov: 0.5}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validBase()
			m.MaxModelLen = tc.maxModelLen
			m.TokenDist = tc.td
			err := validateEntry(&m)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateEntry err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestApplyDefaults_TokenDistMin(t *testing.T) {
	m := validBase()
	m.TokenDist = &tokenDistConfig{Type: "gaussian", Cov: 0.5} // Min unset → 0
	applyDefaults(&m)
	if m.TokenDist.Min != 1 {
		t.Fatalf("TokenDist.Min = %v, want default 1", m.TokenDist.Min)
	}
}
