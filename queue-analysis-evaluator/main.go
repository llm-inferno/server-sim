// queue-analysis-evaluator is a standalone service that implements the
// server-sim evaluator API (POST /solve) using the queue-analysis analytical
// model as its backend. It loads a YAML config file mapping accelerator+model
// pairs to queue-analysis parameters (Alpha, Beta, Gamma, MaxQueueSize).
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
		log.Fatalf("load model data: %v", err)
	}
	log.Printf("loaded %d accelerator/model configurations", len(lookup))

	port := 8081
	if v := os.Getenv("EVALUATOR_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	r := gin.Default()
	r.POST("/solve", solveHandler(lookup))
	if err := r.Run(fmt.Sprintf(":%d", port)); err != nil {
		panic(err)
	}
}
