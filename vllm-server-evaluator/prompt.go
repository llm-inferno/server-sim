package main

import "math/rand"

// syntheticTokenMin/Max defines a small range of "safe" token ids unlikely to
// hit any model's special-token slots. Tokens 100-1099 are well inside the
// regular vocabulary for all current vLLM-supported tokenizers.
const (
	syntheticTokenMin = 100
	syntheticTokenMax = 1099
)

// syntheticPromptTokens returns a slice of `n` randomized token ids. Different
// seeds produce different sequences so concurrent requests don't collide on
// vLLM's prefix cache and inflate TTFT artificially.
func syntheticPromptTokens(n int, seed int64) []int {
	if n <= 0 {
		return []int{}
	}
	r := rand.New(rand.NewSource(seed))
	out := make([]int, n)
	span := syntheticTokenMax - syntheticTokenMin + 1
	for i := range out {
		out[i] = syntheticTokenMin + r.Intn(span)
	}
	return out
}
