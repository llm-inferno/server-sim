package evaluator

import "testing"

func TestResolveMaxConcurrency(t *testing.T) {
	tests := []struct {
		name       string
		requested  int
		configured int
		want       int
	}{
		{"request wins over config", 10, 32, 10},
		{"request wins over backstop", 10, 0, 10},
		{"config used when request unset", 0, 32, 32},
		{"config used when request negative", -1, 32, 32},
		{"backstop when both unset", 0, 0, DefaultMaxConcurrency},
		{"backstop when both non-positive", -5, -1, DefaultMaxConcurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveMaxConcurrency(tt.requested, tt.configured, "test")
			if got != tt.want {
				t.Errorf("ResolveMaxConcurrency(%d, %d) = %d, want %d",
					tt.requested, tt.configured, got, tt.want)
			}
		})
	}
}
