package main

import (
	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/server"
)

func main() {
	cfg := config.Load()
	srv := server.New(cfg)
	srv.Run()
}
