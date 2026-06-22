package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	lookup, err := loadConfig()
	if err != nil {
		log.Fatalf("loadConfig: %v", err)
	}
	log.Printf("continuous-vllm-server-evaluator: loaded %d config entries", len(lookup))

	port := 8081
	if v := os.Getenv("EVALUATOR_PORT"); v != "" {
		if p, perr := strconv.Atoi(v); perr == nil {
			port = p
		}
	}

	vllmPort, perr := resolveVLLMPort(lookup)
	if perr != nil {
		log.Fatalf("%v", perr)
	}

	// Build K8s client once at startup; nil outside a cluster (pairing loop
	// handles nil gracefully by returning an error each iteration).
	var k8sClient kubernetes.Interface
	if cfg, cerr := rest.InClusterConfig(); cerr != nil {
		log.Printf("not in cluster: %v (pairing disabled)", cerr)
	} else if c, kerr := kubernetes.NewForConfig(cfg); kerr != nil {
		log.Printf("k8s client init failed: %v (pairing disabled)", kerr)
	} else {
		k8sClient = c
		log.Printf("k8s client initialized")
	}

	g := newGenerator(lookup)
	// Warmup applies once, anchored at the first accepted arrival (not loop
	// start); reuse the first entry's WarmupSec if set.
	for _, sc := range lookup {
		g.warmup = time.Duration(sc.WarmupSec) * time.Second
		break
	}

	// Background pairing resolver (same cadence as the windowed binary).
	go func() {
		for {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			ps, rerr := resolvePairing(rctx, k8sClient, vllmPort)
			cancel()
			if rerr == nil {
				g.pairing.Store(ps)
				log.Printf("pairing resolved: vLLM pod %s:%d (pair-id=%s)", ps.VLLMPodIP, ps.VLLMPort, ps.PairID)
			} else {
				log.Printf("pairing not yet resolved: %v", rerr)
			}
			time.Sleep(15 * time.Second)
		}
	}()

	// Persistent arrival loop.
	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.runLoop(loopCtx)

	r := gin.Default()
	r.POST("/solve", solveHandler(g))
	log.Printf("continuous-vllm-server-evaluator listening on :%d", port)
	if err := r.Run(fmt.Sprintf(":%d", port)); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// resolveVLLMPort returns the single vLLM port shared by all config entries:
// this evaluator pod is paired with exactly one vLLM pod (one port), even if
// multiple accelerator/model combinations route through it. Entries that omit
// vllmPort (0) are ignored; the default 8000 applies when no entry specifies a
// port. It errors only when two entries specify *different* non-zero ports, and
// is order-independent (map iteration order does not affect the result).
func resolveVLLMPort(lookup map[string]serverConfig) (int, error) {
	port := 0
	for _, sc := range lookup {
		if sc.VLLMPort == 0 {
			continue
		}
		if port == 0 {
			port = sc.VLLMPort
			continue
		}
		if sc.VLLMPort != port {
			return 0, fmt.Errorf("vllm-eval-config has mismatched vllmPort values (%d vs %d); all entries must share the same port", port, sc.VLLMPort)
		}
	}
	if port == 0 {
		port = 8000
	}
	return port, nil
}
