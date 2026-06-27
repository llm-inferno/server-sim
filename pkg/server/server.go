package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
	"github.com/llm-inferno/server-sim/pkg/job"
	"github.com/llm-inferno/server-sim/pkg/noise"
)

// Server is the server-sim REST API server.
type Server struct {
	router     *gin.Engine
	cfg        config.Config
	evalCli    *evaluator.Client
	jobs       *job.Manager
	labelsPath string             // downward-API labels file; used by on-demand /latest
	cancel     context.CancelFunc // cancels the continuous loop; nil when not running
}

// New creates and configures a new Server.
func New(cfg config.Config) *Server {
	s := &Server{
		router:     gin.Default(),
		cfg:        cfg,
		evalCli:    evaluator.NewClient(cfg.EvaluatorURL),
		jobs:       job.NewManager(cfg.JobTTL),
		labelsPath: labelsFilePath(cfg),
	}
	s.router.POST("/simulate", s.handleSimulate)
	s.router.GET("/simulate/:id", s.handleGetJob)
	s.router.GET("/health", s.handleHealth)
	s.router.GET("/latest", s.handleLatest)
	if cfg.ContinuousMode {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		loop := NewLoop(cfg, s.jobs, s.evalCli)
		go loop.Run(ctx)
	}
	return s
}

// Shutdown stops the continuous evaluation loop (and its per-window
// goroutines), if running, and the job store's background sweep. Safe to call
// when continuous mode is disabled, and idempotent.
func (s *Server) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
	s.jobs.Close()
}

// Run starts the HTTP server on the configured port.
func (s *Server) Run() error {
	return s.router.Run(fmt.Sprintf(":%d", s.cfg.Port))
}

// Handler returns the underlying http.Handler, primarily for use in tests.
func (s *Server) Handler() http.Handler {
	return s.router
}

// handleSimulate accepts a ProblemData, creates an async job, and returns the job ID.
func (s *Server) handleSimulate(c *gin.Context) {
	var pd evaluator.ProblemData
	if err := c.ShouldBindJSON(&pd); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	id := s.jobs.Create()

	go func() {
		result, err := s.evalCli.Solve(pd)
		if err != nil {
			s.jobs.Fail(id, err.Error())
			return
		}
		if !result.IsSaturated() && s.cfg.NoiseEnabled {
			result = noise.AddNoise(result, s.cfg.Noise)
			if result.Throughput > pd.RPS {
				result.Throughput = pd.RPS
			}
		}
		s.jobs.Complete(id, pd, result)
	}()

	c.JSON(http.StatusCreated, gin.H{"jobID": id})
}

// handleGetJob returns the current status and result of a simulation job.
func (s *Server) handleGetJob(c *gin.Context) {
	id := c.Param("id")
	j := s.jobs.Get(id)
	if j == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}

	switch j.Status {
	case job.StatusPending:
		c.JSON(http.StatusOK, gin.H{"jobID": j.ID, "status": j.Status})
	case job.StatusCompleted:
		c.JSON(http.StatusOK, gin.H{"jobID": j.ID, "status": j.Status, "result": j.Result})
	case job.StatusFailed:
		c.JSON(http.StatusOK, gin.H{"jobID": j.ID, "status": j.Status, "error": j.Error})
	}
}

// handleHealth responds with 200 OK for liveness checks.
func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleLatest returns the current performance estimate as a self-describing
// envelope. In continuous mode it serves the most-recent loop-completed window
// (a lookback). In non-continuous mode it computes a fresh result on demand from
// the current in-force labels — so effectiveInput.concurrency always equals the
// in-force maxbatchsize and the collector's coherence gate passes by construction.
func (s *Server) handleLatest(c *gin.Context) {
	if s.cfg.ContinuousMode {
		j := s.jobs.Latest()
		if j == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no result yet"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"effectiveInput": j.EffectiveInput,
			"result":         j.Result,
			"completedAt":    j.CompletedAt,
		})
		return
	}

	// Non-continuous (simulator) backend: compute on demand against the current
	// labels. Thread the request context so the collector's GET /latest timeout
	// aborts a too-long solve cleanly rather than orphaning it.
	eff, ad, ok, err := computeLatest(c.Request.Context(), s.cfg.SaturationPolicy, s.evalCli, s.labelsPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no result yet"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"effectiveInput": eff,
		"result":         ad,
		"completedAt":    time.Now().UTC(),
	})
}
