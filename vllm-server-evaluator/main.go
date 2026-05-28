package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

	// All config entries must specify the same vllmPort — this evaluator pod
	// is paired with exactly one vLLM pod (which listens on one port), even
	// if multiple accelerator/model combinations are routed through it.
	vllmPort := 0
	for _, sc := range lookup {
		if vllmPort == 0 {
			vllmPort = sc.VLLMPort
			continue
		}
		if sc.VLLMPort != 0 && sc.VLLMPort != vllmPort {
			log.Fatalf("vllm-eval-config has mismatched vllmPort values (%d vs %d); all entries must share the same port (one paired vLLM per evaluator pod)", vllmPort, sc.VLLMPort)
		}
	}
	if vllmPort == 0 {
		vllmPort = 8000
	}

	// Build K8s client once at startup; nil outside a cluster (pairing loop
	// handles nil gracefully by returning an error each iteration).
	var k8sClient kubernetes.Interface
	if cfg, err := rest.InClusterConfig(); err != nil {
		log.Printf("k8s in-cluster config not available: %v", err)
	} else if c, err := kubernetes.NewForConfig(cfg); err != nil {
		log.Printf("k8s client init failed: %v", err)
	} else {
		k8sClient = c
		log.Printf("k8s client initialized")
	}

	state := &handlerState{Lookup: lookup}

	var pairing atomic.Pointer[pairingState]
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			ps, err := resolvePairing(ctx, k8sClient, vllmPort)
			cancel()
			if err == nil {
				pairing.Store(ps)
				state.Pairing = ps
				log.Printf("pairing resolved: vLLM pod %s:%d (pair-id=%s)", ps.VLLMPodIP, ps.VLLMPort, ps.PairID)
			} else {
				log.Printf("pairing not yet resolved: %v", err)
			}
			time.Sleep(15 * time.Second)
		}
	}()

	r := gin.Default()
	r.POST("/solve", solveHandler(state))
	log.Printf("vllm-server-evaluator listening on :%d", port)
	if err := r.Run(fmt.Sprintf(":%d", port)); err != nil {
		log.Fatalf("vllm-server-evaluator: %v", err)
	}
}
