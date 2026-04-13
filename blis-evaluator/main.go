// blis-evaluator is a standalone service that implements the server-sim
// evaluator API (POST /solve) using inference-sim/BLIS as its DES backend.
// It loads a JSON config file mapping accelerator+model pairs to BLIS
// simulation parameters (KV cache, batch config, model hardware, etc.).
//
// Environment variables:
//
//	BLIS_CONFIG_FILE    path to blis-config.json (default: blis-config.json)
//	HW_CONFIG_FILE      path to hardware_config.json (default: hardware_config.json)
//	LATENCY_BACKEND     BLIS latency model: roofline (default), blackbox, crossmodel,
//	                    trained-roofline, trained-physics
//	EVALUATOR_PORT      HTTP port to listen on (default: 8081)
package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
)

func main() {
	lookup, err := loadConfig()
	if err != nil {
		log.Fatalf("load blis config: %v", err)
	}
	log.Printf("loaded %d accelerator/model configurations", len(lookup))

	backend := os.Getenv("LATENCY_BACKEND")
	if backend == "" {
		backend = "roofline"
	}

	// minBetaCoeffs lists backends that require a minimum number of BetaCoeffs per
	// config entry.  Backends not in this map (roofline) do not use BetaCoeffs.
	minBetaCoeffs := map[string]int{
		"blackbox":         3,
		"crossmodel":       4,
		"trained-roofline": 7,
		"trained-physics":  7,
	}
	validBackends := map[string]bool{
		"roofline": true, "blackbox": true, "crossmodel": true,
		"trained-roofline": true, "trained-physics": true,
	}
	if !validBackends[backend] {
		log.Fatalf("unknown LATENCY_BACKEND %q; valid values: roofline, blackbox, crossmodel, trained-roofline, trained-physics", backend)
	}
	if min, ok := minBetaCoeffs[backend]; ok {
		for key, entry := range lookup {
			if len(entry.BetaCoeffs) < min {
				log.Fatalf("config entry %q: LATENCY_BACKEND=%s requires at least %d betaCoeffs, got %d",
					key, backend, min, len(entry.BetaCoeffs))
			}
		}
	}

	log.Printf("using latency backend: %s", backend)

	port := 8081
	if v := os.Getenv("EVALUATOR_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	r := gin.Default()
	r.POST("/solve", solveHandler(lookup, backend))
	if err := r.Run(fmt.Sprintf(":%d", port)); err != nil {
		panic(err)
	}
}
