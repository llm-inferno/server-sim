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

	r := gin.Default()
	r.POST("/solve", func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "vllm-server evaluator: handler not yet implemented", "configsLoaded": len(lookup)})
	})
	log.Printf("vllm-server-evaluator listening on :%d", port)
	if err := r.Run(fmt.Sprintf(":%d", port)); err != nil {
		panic(err)
	}
}
