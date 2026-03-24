// blis-evaluator is a standalone service that implements the server-sim
// evaluator API (POST /solve) using inference-sim/BLIS as its DES backend.
// It loads a JSON config file mapping accelerator+model pairs to BLIS
// simulation parameters (KV cache, batch config, model hardware, etc.).
//
// Environment variables:
//
//	BLIS_CONFIG_FILE    path to blis-config.json (default: blis-config.json)
//	HW_CONFIG_FILE      path to hardware_config.json (default: hardware_config.json)
//	LATENCY_BACKEND     BLIS latency model: roofline (default), blackbox, crossmodel, trained-roofline
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
