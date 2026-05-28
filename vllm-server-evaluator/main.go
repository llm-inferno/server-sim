package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

func main() {
	lookup, err := loadConfig()
	if err != nil {
		log.Fatalf("load vllm eval config: %v", err)
	}
	log.Printf("loaded %d accelerator/model configurations", len(lookup))

	port := 8081
	if v := os.Getenv("EVALUATOR_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	// Pairing port comes from the FIRST config entry's vllmPort (all entries
	// in a single sidecar pod target the same vLLM, so values must match).
	vllmPort := 8000
	for _, sc := range lookup {
		if sc.VLLMPort > 0 {
			vllmPort = sc.VLLMPort
		}
		break
	}

	var pairing atomic.Pointer[pairingState]
	go func() {
		// Best-effort resolution loop — Actuator may not have written labels yet.
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			ps, err := resolvePairing(ctx, vllmPort)
			cancel()
			if err == nil {
				pairing.Store(ps)
				log.Printf("pairing resolved: vLLM pod %s:%d (pair-id=%s)", ps.VLLMPodIP, ps.VLLMPort, ps.PairID)
			} else {
				log.Printf("pairing not yet resolved: %v", err)
			}
			time.Sleep(15 * time.Second)
		}
	}()

	r := gin.Default()
	r.POST("/solve", func(c *gin.Context) {
		ps := pairing.Load()
		if ps == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vllm pairing not ready"})
			return
		}
		c.JSON(http.StatusNotImplemented, gin.H{"error": "vllm-server evaluator: handler not yet implemented", "vllm": fmt.Sprintf("%s:%d", ps.VLLMPodIP, ps.VLLMPort)})
	})
	log.Printf("vllm-server-evaluator listening on :%d", port)
	if err := r.Run(fmt.Sprintf(":%d", port)); err != nil {
		log.Fatalf("vllm-server-evaluator: %v", err)
	}
}
