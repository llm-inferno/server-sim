package noise

import (
	"math"
	"math/rand"

	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// Config holds noise parameters. StdFraction is the standard deviation
// expressed as a fraction of each metric's value (e.g. 0.05 = 5% noise).
type Config struct {
	StdFraction float64
}

// AddNoise applies zero-mean Gaussian noise to each field of AnalysisData.
// Metrics are clamped to zero to avoid negative values.
func AddNoise(ad evaluator.AnalysisData, cfg Config) evaluator.AnalysisData {
	perturb := func(v float32) float32 {
		if v == 0 {
			return 0
		}
		noisy := float64(v) + rand.NormFloat64()*cfg.StdFraction*float64(v)
		return float32(math.Max(0, noisy))
	}

	return evaluator.AnalysisData{
		Throughput:  perturb(ad.Throughput),
		AvgRespTime: perturb(ad.AvgRespTime),
		AvgWaitTime: perturb(ad.AvgWaitTime),
		AvgTTFT:     perturb(ad.AvgTTFT),
		AvgITL:      perturb(ad.AvgITL),
		MaxRPS:      perturb(ad.MaxRPS),
		Saturation:  ad.Saturation,
	}
}
