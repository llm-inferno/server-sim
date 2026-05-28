// vllm-server-evaluator is a standalone service that implements the server-sim
// evaluator API (POST /solve) by driving a real vLLM server with synthetic
// open-loop traffic. The vLLM pod is paired 1:1 with the managed Deployment pod
// hosting this evaluator via labels written by the control-loop Actuator.
//
// See docs/superpowers/specs/2026-05-28-vllm-server-evaluator-design.md for
// the full design.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
)

func main() {
	port := 8081
	if v := os.Getenv("EVALUATOR_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}

	r := gin.Default()
	r.POST("/solve", func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "vllm-server evaluator: handler not yet implemented"})
	})
	log.Printf("vllm-server-evaluator listening on :%d (stub: returns 501)", port)
	if err := r.Run(fmt.Sprintf(":%d", port)); err != nil {
		panic(err)
	}
}
