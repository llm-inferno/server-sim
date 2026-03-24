package server

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/config"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
	"github.com/llm-inferno/server-sim/pkg/job"
	"github.com/llm-inferno/server-sim/pkg/noise"
)

// Server is the server-sim REST API server.
type Server struct {
	router  *gin.Engine
	cfg     config.Config
	evalCli *evaluator.Client
	jobs    *job.Manager
}

// New creates and configures a new Server.
func New(cfg config.Config) *Server {
	s := &Server{
		router:  gin.Default(),
		cfg:     cfg,
		evalCli: evaluator.NewClient(cfg.EvaluatorURL),
		jobs:    job.NewManager(cfg.JobTTL),
	}
	s.router.POST("/simulate", s.handleSimulate)
	s.router.GET("/simulate/:id", s.handleGetJob)
	s.router.GET("/health", s.handleHealth)
	return s
}

// Run starts the HTTP server on the configured port.
func (s *Server) Run() error {
	return s.router.Run(fmt.Sprintf(":%d", s.cfg.Port))
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
		if s.cfg.NoiseEnabled {
			result = noise.AddNoise(result, s.cfg.Noise)
		}
		s.jobs.Complete(id, result)
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
