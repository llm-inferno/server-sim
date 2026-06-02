package main

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
)

// tokenSampler draws a per-request integer token count (≥ 1).
type tokenSampler interface {
	Sample(*rand.Rand) int
}

type fixedSampler struct{ v int }

func (f fixedSampler) Sample(_ *rand.Rand) int { return f.v }

type geometricSampler struct {
	p  float64 // success probability = 1/avg
	hi int     // upper truncation bound
}

func (g geometricSampler) Sample(rng *rand.Rand) int {
	// X = ceil(log(U) / log(1-p)) is geometric on {1,2,3,...} with mean 1/p.
	// Avoid log(0) by drawing U in (0,1).
	u := rng.Float64()
	if u <= 0 {
		u = math.Nextafter(0, 1)
	}
	n := int(math.Ceil(math.Log(u) / math.Log(1.0-g.p)))
	if n < 1 {
		n = 1
	}
	if n > g.hi {
		n = g.hi
	}
	return n
}

// uniformSampler draws from a discrete uniform on [lo, hi]. Both the
// "uniform" and "uniform-bounded" config kinds construct this type with
// different lo/hi derivations.
type uniformSampler struct{ lo, hi int }

func (u uniformSampler) Sample(rng *rand.Rand) int {
	return u.lo + rng.Intn(u.hi-u.lo+1)
}

// newSampler validates kind and constructs the sampler for the given average.
// avg must be ≥ 1; avg == 1 collapses every kind to fixed{1}.
func newSampler(kind string, avg int) (tokenSampler, error) {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "", "fixed", "geometric", "uniform", "uniform-bounded":
	default:
		return nil, fmt.Errorf("unknown token distribution %q", kind)
	}

	if avg < 1 {
		return nil, fmt.Errorf("token distribution avg must be >= 1, got %d", avg)
	}
	if avg == 1 {
		return fixedSampler{v: 1}, nil
	}

	switch k {
	case "", "fixed":
		return fixedSampler{v: avg}, nil
	case "geometric":
		return geometricSampler{p: 1.0 / float64(avg), hi: 10 * avg}, nil
	case "uniform":
		return uniformSampler{lo: 1, hi: 2*avg - 1}, nil
	case "uniform-bounded":
		// avg ≥ 2 here, so avg/2 ≥ 1 — no further lower-bound clamp needed.
		return uniformSampler{lo: avg / 2, hi: (3*avg + 1) / 2}, nil
	}
	return nil, fmt.Errorf("unreachable") // exhaustive switch above
}
