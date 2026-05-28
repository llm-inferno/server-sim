# vllm-server Evaluator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a fourth evaluator backend, `vllm-server`, that drives a real vLLM server with Poisson open-loop traffic and reports measured TTFT, ITL, response time, throughput, and queue time via the existing `POST /solve(ProblemData) → AnalysisData` contract.

**Architecture:** New `vllm-server-evaluator/` package built as a fourth binary in the existing `evaluator` image, selected via `args: ["vllm-server"]`. The evaluator resolves its paired vLLM pod via the K8s API (label-based pairing maintained by the control-loop Actuator, out of scope for this plan), then for each `/solve` call drives synthetic streaming requests at `pd.RPS` for a configurable window, scrapes vLLM's Prometheus `/metrics`, and aggregates samples into `AnalysisData`.

**Tech Stack:** Go 1.25, Gin, `net/http` for vLLM HTTP/SSE, `k8s.io/client-go` for pairing, stdlib for everything else (no Prometheus exposition library — minimal text-format parser sufficient since we read one histogram metric).

**Spec:** `docs/superpowers/specs/2026-05-28-vllm-server-evaluator-design.md` (commit `d2cfd21`)
**Issue:** llm-inferno/server-sim#7
**Branch:** `feat/vllm-server-evaluator`

---

## PR plan

| # | PR | Scope |
|---|---|---|
| 1 | Scaffolding + dispatch | Binary returning 501; Dockerfile + evaluator.sh wired |
| 2 | Config loader | Config schema, lookup, tests |
| 3 | Pairing resolver | Downward-API + K8s client (adds client-go dep); tests with fake client |
| 4 | Generator + metrics scrape | Poisson driver, SSE parsing, /metrics scrape |
| 5 | Saturation + /solve handler | Saturation detection + full /solve integration |
| 6 | K8s manifests + docs | Manifests, RBAC, contract doc, README/CLAUDE.md updates |

Each PR ends with `gh pr create` referencing `Refs #7`. Land in order — later PRs build on earlier ones.

---

## PR 1: Scaffolding + Dockerfile dispatch

Establishes the binary so subsequent PRs have something to wire into.

### Task 1.1: Create empty binary that returns 501 from /solve

**Files:**
- Create: `vllm-server-evaluator/main.go`

- [ ] **Step 1: Write `vllm-server-evaluator/main.go`**

```go
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
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./vllm-server-evaluator`
Expected: exits 0; binary `vllm-server-evaluator` produced in CWD; `rm vllm-server-evaluator` to clean up.

- [ ] **Step 3: Verify the whole module still builds**

Run: `go build ./...`
Expected: exits 0.

- [ ] **Step 4: Commit**

```bash
git add vllm-server-evaluator/main.go
git commit -m "feat(vllm-server-evaluator): scaffold binary stub returning 501

Refs #7"
```

### Task 1.2: Wire into Dockerfile.evaluator and evaluator.sh

**Files:**
- Modify: `Dockerfile.evaluator`
- Modify: `evaluator.sh`

- [ ] **Step 1: Add build step to `Dockerfile.evaluator`**

Edit `Dockerfile.evaluator`. After the existing `RUN go build -o blis-evaluator ./blis-evaluator` line, add:

```dockerfile
RUN go build -o vllm-server-evaluator ./vllm-server-evaluator
```

After the existing `COPY --from=builder /app/blis-evaluator .` line in the alpine stage, add:

```dockerfile
COPY --from=builder /app/vllm-server-evaluator .
```

- [ ] **Step 2: Add dispatch case to `evaluator.sh`**

Replace the current case block in `evaluator.sh` with:

```sh
#!/bin/sh
set -e
case "$1" in
  dummy)           exec ./dummy-evaluator ;;
  queue-analysis)  exec ./queue-analysis-evaluator ;;
  blis)            exec ./blis-evaluator ;;
  vllm-server)     exec ./vllm-server-evaluator ;;
  *)               echo "Unknown backend: $1. Use: dummy, queue-analysis, blis, vllm-server" >&2; exit 1 ;;
esac
```

- [ ] **Step 3: Build the evaluator image to confirm**

Run: `docker build -f Dockerfile.evaluator -t evaluator:vllm-test .`
Expected: build completes successfully; the image now contains all four binaries.

- [ ] **Step 4: Smoke-test the dispatch**

Run: `docker run --rm -p 18081:8081 evaluator:vllm-test vllm-server &`
Then in another shell: `curl -sS -X POST http://localhost:18081/solve -d '{}' -H 'Content-Type: application/json'`
Expected: returns 501 with body `{"error":"vllm-server evaluator: handler not yet implemented"}`.
Cleanup: `docker stop $(docker ps -q --filter ancestor=evaluator:vllm-test)`.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile.evaluator evaluator.sh
git commit -m "build(evaluator): include vllm-server binary in image dispatch

Refs #7"
```

### Task 1.3: Open PR 1

- [ ] **Step 1: Push branch and open PR**

```bash
git push -u origin feat/vllm-server-evaluator

gh pr create --title "feat(vllm-server-evaluator): PR 1/6 - scaffolding + image dispatch" --body "$(cat <<'EOF'
## Summary

PR 1 of 6 implementing the vllm-server evaluator backend.

- New `vllm-server-evaluator/` package with a stub `/solve` returning 501
- `Dockerfile.evaluator` and `evaluator.sh` extended to build and dispatch the new binary

This PR establishes the binary so subsequent PRs (config, pairing, generator, handler) have something to wire into. No real functionality yet.

## Test plan

- [x] `go build ./...` succeeds
- [x] `docker build -f Dockerfile.evaluator` succeeds
- [x] `docker run ... vllm-server` followed by `curl /solve` returns 501

Refs #7
EOF
)"
```

After PR 1 merges to main, rebase the feature branch on the new main before continuing PR 2:

```bash
git fetch origin main
git rebase origin/main
```

---

## PR 2: Config loader

### Task 2.1: Define config types and write the test

**Files:**
- Create: `vllm-server-evaluator/config.go`
- Create: `vllm-server-evaluator/config_test.go`
- Create: `vllm-server-evaluator/vllm-eval-config.json` (sample)

- [ ] **Step 1: Write the failing test in `config_test.go`**

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
		"configs": [
			{
				"accelerator": "H100",
				"model": "ibm-granite/granite-3.1-8b-instruct",
				"vllmServedModelName": "granite",
				"vllmPort": 8000,
				"warmupSec": 5,
				"minWindowSec": 20,
				"maxWindowSec": 300,
				"targetSamples": 200,
				"minSamples": 50,
				"ignoreEOS": true,
				"queueTimeMetric": "vllm:request_queue_time_seconds"
			}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)

	lookup, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(lookup) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(lookup))
	}
	got, ok := lookup["H100|ibm-granite/granite-3.1-8b-instruct"]
	if !ok {
		t.Fatalf("missing expected key; have keys: %v", keysOf(lookup))
	}
	if got.VLLMServedModelName != "granite" {
		t.Errorf("VLLMServedModelName = %q, want %q", got.VLLMServedModelName, "granite")
	}
	if got.VLLMPort != 8000 {
		t.Errorf("VLLMPort = %d, want 8000", got.VLLMPort)
	}
	if got.WarmupSec != 5 {
		t.Errorf("WarmupSec = %d, want 5", got.WarmupSec)
	}
	if got.IgnoreEOS != true {
		t.Errorf("IgnoreEOS = %v, want true", got.IgnoreEOS)
	}
}

func TestLoadConfig_DefaultsServedModelName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{
		"configs": [
			{ "accelerator": "H100", "model": "m", "vllmPort": 8000,
			  "warmupSec": 1, "minWindowSec": 1, "maxWindowSec": 10,
			  "targetSamples": 10, "minSamples": 5,
			  "queueTimeMetric": "vllm:request_queue_time_seconds" }
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)

	lookup, _ := loadConfig()
	got := lookup["H100|m"]
	if got.VLLMServedModelName != "m" {
		t.Errorf("VLLMServedModelName fallback to model = %q, want %q", got.VLLMServedModelName, "m")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	t.Setenv("VLLM_EVAL_CONFIG_FILE", "/nonexistent/path.json")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VLLM_EVAL_CONFIG_FILE", path)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func keysOf(m map[string]serverConfig) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
```

- [ ] **Step 2: Run the test and confirm it fails to compile**

Run: `go test ./vllm-server-evaluator/...`
Expected: build error — `loadConfig` and `serverConfig` undefined.

- [ ] **Step 3: Implement `vllm-server-evaluator/config.go`**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// configEntry is one entry in vllm-eval-config.json.
type configEntry struct {
	Accelerator         string `json:"accelerator"`
	Model               string `json:"model"`
	VLLMServedModelName string `json:"vllmServedModelName"`
	VLLMPort            int    `json:"vllmPort"`
	WarmupSec           int    `json:"warmupSec"`
	MinWindowSec        int    `json:"minWindowSec"`
	MaxWindowSec        int    `json:"maxWindowSec"`
	TargetSamples       int    `json:"targetSamples"`
	MinSamples          int    `json:"minSamples"`
	IgnoreEOS           bool   `json:"ignoreEOS"`
	QueueTimeMetric     string `json:"queueTimeMetric"`
}

// configFile is the top-level structure of vllm-eval-config.json.
type configFile struct {
	Configs []configEntry `json:"configs"`
}

// serverConfig is the validated, lookup-ready measurement policy for one
// (accelerator, model) pair.
type serverConfig struct {
	VLLMServedModelName string
	VLLMPort            int
	WarmupSec           int
	MinWindowSec        int
	MaxWindowSec        int
	TargetSamples       int
	MinSamples          int
	IgnoreEOS           bool
	QueueTimeMetric     string
}

// loadConfig reads vllm-eval-config.json from VLLM_EVAL_CONFIG_FILE
// (default: vllm-eval-config.json) and returns a lookup map keyed
// by "accelerator|model".
//
// VLLMServedModelName defaults to the model name when empty.
func loadConfig() (map[string]serverConfig, error) {
	path := os.Getenv("VLLM_EVAL_CONFIG_FILE")
	if path == "" {
		path = "vllm-eval-config.json"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vllm eval config %q: %w", path, err)
	}

	var cf configFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse vllm eval config %q: %w", path, err)
	}

	lookup := make(map[string]serverConfig, len(cf.Configs))
	for _, e := range cf.Configs {
		served := e.VLLMServedModelName
		if served == "" {
			served = e.Model
		}
		lookup[e.Accelerator+"|"+e.Model] = serverConfig{
			VLLMServedModelName: served,
			VLLMPort:            e.VLLMPort,
			WarmupSec:           e.WarmupSec,
			MinWindowSec:        e.MinWindowSec,
			MaxWindowSec:        e.MaxWindowSec,
			TargetSamples:       e.TargetSamples,
			MinSamples:          e.MinSamples,
			IgnoreEOS:           e.IgnoreEOS,
			QueueTimeMetric:     e.QueueTimeMetric,
		}
	}
	return lookup, nil
}
```

- [ ] **Step 4: Run tests — expect PASS**

Run: `go test ./vllm-server-evaluator/... -v`
Expected: all four tests pass.

- [ ] **Step 5: Create sample config**

Write `vllm-server-evaluator/vllm-eval-config.json`:

```json
{
  "configs": [
    {
      "accelerator": "H100",
      "model": "ibm-granite/granite-3.1-8b-instruct",
      "vllmServedModelName": "granite",
      "vllmPort": 8000,
      "warmupSec": 5,
      "minWindowSec": 20,
      "maxWindowSec": 300,
      "targetSamples": 200,
      "minSamples": 50,
      "ignoreEOS": true,
      "queueTimeMetric": "vllm:request_queue_time_seconds"
    }
  ]
}
```

- [ ] **Step 6: Wire config load into `main.go`**

Replace `vllm-server-evaluator/main.go` with:

```go
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
```

- [ ] **Step 7: Verify everything builds and tests pass**

Run: `go build ./... && go test ./vllm-server-evaluator/... -v`
Expected: build succeeds, all tests pass.

- [ ] **Step 8: Commit**

```bash
git add vllm-server-evaluator/config.go vllm-server-evaluator/config_test.go vllm-server-evaluator/vllm-eval-config.json vllm-server-evaluator/main.go
git commit -m "feat(vllm-server-evaluator): config loader and sample config

Reads vllm-eval-config.json keyed by accelerator|model, with measurement
policy fields (warmup, window, samples). vllmServedModelName defaults
to the model name when empty.

Refs #7"
```

### Task 2.2: Open PR 2

- [ ] **Step 1: Push and open PR**

```bash
git push origin feat/vllm-server-evaluator

gh pr create --title "feat(vllm-server-evaluator): PR 2/6 - config loader" --body "$(cat <<'EOF'
## Summary

PR 2 of 6. Adds config loading for the vllm-server evaluator.

- `vllm-eval-config.json` schema keyed by `accelerator|model`
- Loader with sensible default for `vllmServedModelName`
- Tests cover valid, missing file, invalid JSON, and default-served-model paths

## Test plan

- [x] `go test ./vllm-server-evaluator/...` passes (4 tests)

Refs #7
EOF
)"
```

---

## PR 3: Pairing resolver (downward API + K8s client)

### Task 3.1: Add k8s.io/client-go dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the dependency**

Run: `cd /Users/tantawi/Projects/llm-inferno/server-sim && go get k8s.io/client-go@v0.31.0 k8s.io/apimachinery@v0.31.0`
Expected: `go.mod` updated; `go.sum` updated. (Use the latest minor stable available — pin to a single minor version.)

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: exits 0.

- [ ] **Step 3: Commit dep bump**

```bash
git add go.mod go.sum
git commit -m "build: add k8s.io/client-go for vllm-server-evaluator pairing

Refs #7"
```

### Task 3.2: Implement label-file reader

**Files:**
- Create: `vllm-server-evaluator/pairing.go`
- Create: `vllm-server-evaluator/pairing_test.go`

- [ ] **Step 1: Write the failing test for `readDownwardLabel`**

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDownwardLabel_Present(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pair-id"), []byte("uuid-abc-123"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readDownwardLabel(dir, "pair-id")
	if err != nil {
		t.Fatalf("readDownwardLabel: %v", err)
	}
	if got != "uuid-abc-123" {
		t.Errorf("got %q, want %q", got, "uuid-abc-123")
	}
}

func TestReadDownwardLabel_StripsWhitespace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pair-id"), []byte("uuid-abc-123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := readDownwardLabel(dir, "pair-id")
	if got != "uuid-abc-123" {
		t.Errorf("got %q, want %q (newline should be stripped)", got, "uuid-abc-123")
	}
}

func TestReadDownwardLabel_Missing(t *testing.T) {
	dir := t.TempDir()
	if _, err := readDownwardLabel(dir, "pair-id"); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
```

- [ ] **Step 2: Run — confirm fails to compile**

Run: `go test ./vllm-server-evaluator/...`
Expected: `readDownwardLabel` undefined.

- [ ] **Step 3: Create `pairing.go` with `readDownwardLabel`**

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readDownwardLabel reads a single value from a pod's downward-API volume.
// The volume is conventionally mounted at /etc/podinfo and contains one file
// per requested fieldRef.
func readDownwardLabel(dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("read downward label %s/%s: %w", dir, name, err)
	}
	return strings.TrimSpace(string(data)), nil
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -run TestReadDownwardLabel -v`
Expected: 3 tests pass.

- [ ] **Step 5: Commit**

```bash
git add vllm-server-evaluator/pairing.go vllm-server-evaluator/pairing_test.go
git commit -m "feat(vllm-server-evaluator): downward-API label reader

Refs #7"
```

### Task 3.3: Implement K8s pod resolver with fake client tests

**Files:**
- Modify: `vllm-server-evaluator/pairing.go`
- Modify: `vllm-server-evaluator/pairing_test.go`

- [ ] **Step 1: Append failing tests for `resolvePairedVLLM`**

Append to `vllm-server-evaluator/pairing_test.go`:

```go
import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func makePod(name, namespace, pairID string, ready bool, ip string) *corev1.Pod {
	condStatus := corev1.ConditionFalse
	if ready {
		condStatus = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"inferno.server.pair-id": pairID},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: condStatus},
			},
		},
	}
}

func TestResolvePairedVLLM_Found(t *testing.T) {
	c := fake.NewSimpleClientset(
		makePod("vllm-1", "vllm-ns", "uuid-A", true, "10.0.0.1"),
		makePod("vllm-2", "vllm-ns", "uuid-B", true, "10.0.0.2"),
	)
	ip, err := resolvePairedVLLM(context.Background(), c, "vllm-ns", "uuid-A")
	if err != nil {
		t.Fatalf("resolvePairedVLLM: %v", err)
	}
	if ip != "10.0.0.1" {
		t.Errorf("got %q, want %q", ip, "10.0.0.1")
	}
}

func TestResolvePairedVLLM_NoMatch(t *testing.T) {
	c := fake.NewSimpleClientset(
		makePod("vllm-1", "vllm-ns", "uuid-OTHER", true, "10.0.0.1"),
	)
	_, err := resolvePairedVLLM(context.Background(), c, "vllm-ns", "uuid-A")
	if err == nil {
		t.Fatal("expected error for no matching pod, got nil")
	}
}

func TestResolvePairedVLLM_NotReady(t *testing.T) {
	c := fake.NewSimpleClientset(
		makePod("vllm-1", "vllm-ns", "uuid-A", false, "10.0.0.1"),
	)
	_, err := resolvePairedVLLM(context.Background(), c, "vllm-ns", "uuid-A")
	if err == nil {
		t.Fatal("expected error for not-ready pod, got nil")
	}
}

func TestResolvePairedVLLM_Multiple(t *testing.T) {
	c := fake.NewSimpleClientset(
		makePod("vllm-1", "vllm-ns", "uuid-A", true, "10.0.0.1"),
		makePod("vllm-2", "vllm-ns", "uuid-A", true, "10.0.0.2"),
	)
	_, err := resolvePairedVLLM(context.Background(), c, "vllm-ns", "uuid-A")
	if err == nil {
		t.Fatal("expected error for multiple matches, got nil")
	}
}
```

Add `import "context"` to the existing import block. (Go complains if you try to add a `context` import alongside the appended `import (...)`. Merge the imports manually or move the new imports into the existing block.)

- [ ] **Step 2: Run — confirm fails to compile**

Run: `go test ./vllm-server-evaluator/...`
Expected: `resolvePairedVLLM` undefined.

- [ ] **Step 3: Add `resolvePairedVLLM` to `pairing.go`**

Append to `vllm-server-evaluator/pairing.go`:

```go
import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// resolvePairedVLLM finds the vLLM pod paired with this evaluator by listing
// pods in the given namespace with label inferno.server.pair-id=<pairID>,
// filtering to those in PodRunning phase with PodReady=True. Expects exactly
// one match.
func resolvePairedVLLM(ctx context.Context, c kubernetes.Interface, namespace, pairID string) (string, error) {
	pods, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "inferno.server.pair-id=" + pairID,
	})
	if err != nil {
		return "", fmt.Errorf("list pods in %s with pair-id=%s: %w", namespace, pairID, err)
	}

	ready := make([]corev1.Pod, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		isReady := false
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}
		if isReady && p.Status.PodIP != "" {
			ready = append(ready, p)
		}
	}

	switch len(ready) {
	case 0:
		return "", fmt.Errorf("no Ready vLLM pod with pair-id=%s in namespace %s", pairID, namespace)
	case 1:
		return ready[0].Status.PodIP, nil
	default:
		return "", fmt.Errorf("multiple (%d) Ready vLLM pods with pair-id=%s in namespace %s", len(ready), pairID, namespace)
	}
}
```

Merge the new `import` block into the existing one at the top of `pairing.go` (don't leave two import blocks).

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -v`
Expected: 4 new tests pass.

- [ ] **Step 5: Commit**

```bash
git add vllm-server-evaluator/pairing.go vllm-server-evaluator/pairing_test.go
git commit -m "feat(vllm-server-evaluator): K8s pod resolver for paired vLLM

Lists pods by pair-id label, filters to Running+Ready, errors on
zero or multiple matches.

Refs #7"
```

### Task 3.4: Wire pairing resolution into main.go

**Files:**
- Modify: `vllm-server-evaluator/main.go`

- [ ] **Step 1: Add a `pairingState` struct and resolver call**

Add to `vllm-server-evaluator/pairing.go` (so it lives with related logic):

```go
import (
	"k8s.io/client-go/rest"
)

// pairingState is the cached pairing info this evaluator uses to reach its vLLM.
type pairingState struct {
	PairID         string
	VLLMNamespace  string
	VLLMDeployment string
	VLLMPodIP      string  // empty until resolved
	VLLMPort       int
}

const downwardLabelDir = "/etc/podinfo"

// resolvePairing reads the downward-API labels and looks up the paired vLLM
// pod. Returns a populated pairingState, or an error in unpaired/cold-start
// conditions (caller should treat as 503-pending).
func resolvePairing(ctx context.Context, port int) (*pairingState, error) {
	pairID, err := readDownwardLabel(downwardLabelDir, "pair-id")
	if err != nil || pairID == "" {
		return nil, fmt.Errorf("pair-id not present in downward labels: %v", err)
	}
	vllmDep, err := readDownwardLabel(downwardLabelDir, "vllm-deployment")
	if err != nil || vllmDep == "" {
		return nil, fmt.Errorf("vllm-deployment not present in downward labels: %v", err)
	}

	ns := os.Getenv("VLLM_NAMESPACE")
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	if ns == "" {
		return nil, fmt.Errorf("VLLM_NAMESPACE and POD_NAMESPACE both unset")
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}

	ip, err := resolvePairedVLLM(ctx, client, ns, pairID)
	if err != nil {
		return nil, err
	}
	return &pairingState{
		PairID:         pairID,
		VLLMNamespace:  ns,
		VLLMDeployment: vllmDep,
		VLLMPodIP:      ip,
		VLLMPort:       port,
	}, nil
}
```

- [ ] **Step 2: Modify `main.go` to resolve pairing at startup (best-effort)**

Replace `vllm-server-evaluator/main.go` with:

```go
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
		panic(err)
	}
}
```

- [ ] **Step 3: Build to verify**

Run: `go build ./...`
Expected: exits 0.

- [ ] **Step 4: Run all package tests**

Run: `go test ./vllm-server-evaluator/... -v`
Expected: all existing tests still pass.

- [ ] **Step 5: Commit**

```bash
git add vllm-server-evaluator/pairing.go vllm-server-evaluator/main.go
git commit -m "feat(vllm-server-evaluator): wire pairing resolution into main

Background goroutine resolves pairing every 15s. /solve returns 503
until pairing is ready.

Refs #7"
```

### Task 3.5: Open PR 3

- [ ] **Step 1: Push and open PR**

```bash
git push origin feat/vllm-server-evaluator

gh pr create --title "feat(vllm-server-evaluator): PR 3/6 - pairing resolver" --body "$(cat <<'EOF'
## Summary

PR 3 of 6. Adds the K8s-API-based pairing resolver.

- Reads `pair-id` and `vllm-deployment` from downward API at `/etc/podinfo`
- Lists pods in `VLLM_NAMESPACE` matching `inferno.server.pair-id=<uuid>`, filters to Running+Ready
- Background goroutine retries every 15s; `/solve` returns 503 until ready
- Tests use `k8s.io/client-go/kubernetes/fake` (zero, single, multiple, not-ready cases)
- Adds `k8s.io/client-go` and `k8s.io/apimachinery` deps

## Test plan

- [x] `go test ./vllm-server-evaluator/...` passes (7 tests)
- [x] `go build ./...` succeeds

Refs #7
EOF
)"
```

---

## PR 4: Generator + metrics scrape

This PR adds the load-driving and metrics-collection logic, tested with a `httptest.Server` fake vLLM. Does not yet wire into `/solve` — that happens in PR 5.

### Task 4.1: Define internal types

**Files:**
- Create: `vllm-server-evaluator/types.go`

- [ ] **Step 1: Write `vllm-server-evaluator/types.go`**

```go
package main

import "time"

// requestSpec is the synthetic request the generator sends to vLLM.
type requestSpec struct {
	InputTokens  int
	OutputTokens int
	IgnoreEOS    bool
}

// sample is one completed measurement from a single request.
type sample struct {
	StartedAt    time.Time
	TTFT         time.Duration
	ITLs         []time.Duration // per-chunk inter-arrival deltas after first chunk
	ResponseTime time.Duration
	Failed       bool   // network or server error
	StatusCode   int    // 0 if no response received
}

// windowResult is the aggregated outcome of a measurement window.
type windowResult struct {
	WindowStart   time.Time
	WindowEnd     time.Time
	Samples       []sample
	WarmupSamples int           // count of pre-window samples discarded
	ScrapeAtEnd   metricsScrape // /metrics scraped at WindowEnd
	ScrapeAtStart metricsScrape // /metrics scraped at WindowStart (for delta math)
}

// metricsScrape is the minimal slice of vLLM /metrics we read.
type metricsScrape struct {
	QueueTimeSum   float64 // vllm:request_queue_time_seconds_sum
	QueueTimeCount float64 // vllm:request_queue_time_seconds_count
	InferTimeSum   float64 // vllm:request_inference_time_seconds_sum
	InferTimeCount float64 // vllm:request_inference_time_seconds_count
}
```

- [ ] **Step 2: Build to confirm**

Run: `go build ./vllm-server-evaluator/`
Expected: exits 0.

- [ ] **Step 3: Commit**

```bash
git add vllm-server-evaluator/types.go
git commit -m "feat(vllm-server-evaluator): internal sample/window types

Refs #7"
```

### Task 4.2: Prometheus text-format parser (minimal)

**Files:**
- Create: `vllm-server-evaluator/metrics.go`
- Create: `vllm-server-evaluator/metrics_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScrapeMetrics_Parses(t *testing.T) {
	body := `# HELP vllm:request_queue_time_seconds Request queue time
# TYPE vllm:request_queue_time_seconds histogram
vllm:request_queue_time_seconds_bucket{le="0.1"} 5
vllm:request_queue_time_seconds_bucket{le="+Inf"} 10
vllm:request_queue_time_seconds_sum 1.25
vllm:request_queue_time_seconds_count 10
# HELP vllm:request_inference_time_seconds Inference time
# TYPE vllm:request_inference_time_seconds histogram
vllm:request_inference_time_seconds_sum 5.0
vllm:request_inference_time_seconds_count 10
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := scrapeMetrics(context.Background(), srv.URL+"/metrics")
	if err != nil {
		t.Fatalf("scrapeMetrics: %v", err)
	}
	if got.QueueTimeSum != 1.25 {
		t.Errorf("QueueTimeSum = %v, want 1.25", got.QueueTimeSum)
	}
	if got.QueueTimeCount != 10 {
		t.Errorf("QueueTimeCount = %v, want 10", got.QueueTimeCount)
	}
	if got.InferTimeSum != 5.0 {
		t.Errorf("InferTimeSum = %v, want 5.0", got.InferTimeSum)
	}
	if got.InferTimeCount != 10 {
		t.Errorf("InferTimeCount = %v, want 10", got.InferTimeCount)
	}
}

func TestScrapeMetrics_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := scrapeMetrics(context.Background(), srv.URL+"/metrics"); err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}
```

- [ ] **Step 2: Run — confirm fails to compile**

Run: `go test ./vllm-server-evaluator/...`
Expected: `scrapeMetrics` undefined.

- [ ] **Step 3: Implement `metrics.go`**

```go
package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// scrapeMetrics fetches and parses vLLM's Prometheus /metrics endpoint,
// extracting queue-time and inference-time histogram aggregates. Other
// metrics in the response are ignored. Uses a 5s timeout.
func scrapeMetrics(ctx context.Context, url string) (metricsScrape, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return metricsScrape{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return metricsScrape{}, fmt.Errorf("scrape %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return metricsScrape{}, fmt.Errorf("scrape %s: status %d", url, resp.StatusCode)
	}

	var m metricsScrape
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// We only care about four exact metric names (no labels).
		switch {
		case strings.HasPrefix(line, "vllm:request_queue_time_seconds_sum "):
			m.QueueTimeSum = parseValue(line)
		case strings.HasPrefix(line, "vllm:request_queue_time_seconds_count "):
			m.QueueTimeCount = parseValue(line)
		case strings.HasPrefix(line, "vllm:request_inference_time_seconds_sum "):
			m.InferTimeSum = parseValue(line)
		case strings.HasPrefix(line, "vllm:request_inference_time_seconds_count "):
			m.InferTimeCount = parseValue(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return metricsScrape{}, fmt.Errorf("scan /metrics: %w", err)
	}
	return m, nil
}

// parseValue extracts the float value from a prom text line "metricname value".
// Returns 0 on parse failure (caller treats zero as "missing").
func parseValue(line string) float64 {
	idx := strings.LastIndex(line, " ")
	if idx < 0 {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line[idx+1:]), 64)
	if err != nil {
		return 0
	}
	return v
}

// windowDelta returns the per-completion mean of a histogram over a window:
// (sum_end - sum_start) / (count_end - count_start), in seconds. Returns 0
// if the count delta is non-positive.
func windowDelta(start, end float64, startCount, endCount float64) float64 {
	dc := endCount - startCount
	if dc <= 0 {
		return 0
	}
	return (end - start) / dc
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -run TestScrape -v`
Expected: 2 tests pass.

- [ ] **Step 5: Commit**

```bash
git add vllm-server-evaluator/metrics.go vllm-server-evaluator/metrics_test.go
git commit -m "feat(vllm-server-evaluator): /metrics scraper and window-delta helper

Minimal Prometheus text-format parser pulling four named series; no
external dep.

Refs #7"
```

### Task 4.3: Synthetic prompt builder

**Files:**
- Create: `vllm-server-evaluator/prompt.go`
- Create: `vllm-server-evaluator/prompt_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
)

func TestSyntheticPromptTokens_LengthAndRange(t *testing.T) {
	tokens := syntheticPromptTokens(128, 42)
	if len(tokens) != 128 {
		t.Fatalf("len = %d, want 128", len(tokens))
	}
	for _, tok := range tokens {
		if tok < syntheticTokenMin || tok > syntheticTokenMax {
			t.Errorf("token %d out of range [%d,%d]", tok, syntheticTokenMin, syntheticTokenMax)
		}
	}
}

func TestSyntheticPromptTokens_VariesAcrossSeeds(t *testing.T) {
	a := syntheticPromptTokens(64, 1)
	b := syntheticPromptTokens(64, 2)
	identical := true
	for i := range a {
		if a[i] != b[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Error("expected different tokens for different seeds (avoids prefix-cache hits)")
	}
}

func TestSyntheticPromptTokens_ZeroLength(t *testing.T) {
	if len(syntheticPromptTokens(0, 1)) != 0 {
		t.Error("zero length input should produce empty slice")
	}
}
```

- [ ] **Step 2: Run — confirm fails to compile**

Run: `go test ./vllm-server-evaluator/... -run TestSynthetic`
Expected: undefined symbols.

- [ ] **Step 3: Implement `prompt.go`**

```go
package main

import "math/rand"

// syntheticTokenMin/Max defines a small range of "safe" token ids unlikely to
// hit any model's special-token slots. Tokens 100-1099 are well inside the
// regular vocabulary for all current vLLM-supported tokenizers.
const (
	syntheticTokenMin = 100
	syntheticTokenMax = 1099
)

// syntheticPromptTokens returns a slice of `n` randomized token ids. Different
// seeds produce different sequences so concurrent requests don't collide on
// vLLM's prefix cache and inflate TTFT artificially.
func syntheticPromptTokens(n int, seed int64) []int {
	if n <= 0 {
		return []int{}
	}
	r := rand.New(rand.NewSource(seed))
	out := make([]int, n)
	span := syntheticTokenMax - syntheticTokenMin + 1
	for i := range out {
		out[i] = syntheticTokenMin + r.Intn(span)
	}
	return out
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -run TestSynthetic -v`
Expected: 3 tests pass.

- [ ] **Step 5: Commit**

```bash
git add vllm-server-evaluator/prompt.go vllm-server-evaluator/prompt_test.go
git commit -m "feat(vllm-server-evaluator): synthetic prompt token builder

Per-request randomized tokens in a small safe range to avoid vLLM
prefix-cache hits skewing TTFT.

Refs #7"
```

### Task 4.4: Single-request streaming driver

**Files:**
- Create: `vllm-server-evaluator/generator.go`
- Create: `vllm-server-evaluator/generator_test.go`

- [ ] **Step 1: Write the failing test for `runOneRequest`**

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeVLLMServer returns an httptest.Server that emits N SSE chunks at the
// given inter-chunk interval, then sends [DONE].
func fakeVLLMServer(t *testing.T, numChunks int, interval time.Duration, firstChunkDelay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/completions") {
			http.NotFound(w, r)
			return
		}
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(firstChunkDelay)
		for i := 0; i < numChunks; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"text\":\"x\"}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			if i < numChunks-1 {
				time.Sleep(interval)
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

func TestRunOneRequest_TTFTandITL(t *testing.T) {
	srv := fakeVLLMServer(t, 4, 50*time.Millisecond, 100*time.Millisecond)
	defer srv.Close()

	spec := requestSpec{InputTokens: 8, OutputTokens: 4, IgnoreEOS: true}
	s := runOneRequest(context.Background(), srv.URL, "test-model", spec, 1)

	if s.Failed {
		t.Fatalf("request failed: status=%d", s.StatusCode)
	}
	if s.TTFT < 90*time.Millisecond || s.TTFT > 200*time.Millisecond {
		t.Errorf("TTFT = %v, want ~100ms", s.TTFT)
	}
	if len(s.ITLs) != 3 { // 4 chunks → 3 inter-arrival deltas
		t.Errorf("ITLs len = %d, want 3", len(s.ITLs))
	}
	for _, itl := range s.ITLs {
		if itl < 30*time.Millisecond || itl > 100*time.Millisecond {
			t.Errorf("ITL = %v, want ~50ms", itl)
		}
	}
}

func TestRunOneRequest_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	spec := requestSpec{InputTokens: 4, OutputTokens: 2, IgnoreEOS: true}
	s := runOneRequest(context.Background(), srv.URL, "test-model", spec, 1)
	if !s.Failed {
		t.Errorf("expected Failed=true on 500")
	}
	if s.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", s.StatusCode)
	}
}
```

- [ ] **Step 2: Run — confirm undefined**

Run: `go test ./vllm-server-evaluator/...`
Expected: `runOneRequest` undefined.

- [ ] **Step 3: Implement `generator.go` (single-request)**

```go
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// completionsRequest mirrors vLLM's OpenAI-compatible /v1/completions body.
type completionsRequest struct {
	Model          string `json:"model"`
	PromptTokenIDs []int  `json:"prompt_token_ids"`
	MaxTokens      int    `json:"max_tokens"`
	IgnoreEOS      bool   `json:"ignore_eos"`
	Stream         bool   `json:"stream"`
}

// runOneRequest sends a single streaming /v1/completions request to the given
// vLLM base URL and returns the per-request sample. Caller is responsible for
// scheduling (Poisson) and for filtering out warmup samples.
//
// vllmBaseURL is e.g. "http://10.0.0.1:8000".
func runOneRequest(ctx context.Context, vllmBaseURL, model string, spec requestSpec, seed int64) sample {
	body := completionsRequest{
		Model:          model,
		PromptTokenIDs: syntheticPromptTokens(spec.InputTokens, seed),
		MaxTokens:      spec.OutputTokens,
		IgnoreEOS:      spec.IgnoreEOS,
		Stream:         true,
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vllmBaseURL+"/v1/completions", bytes.NewReader(buf))
	if err != nil {
		return sample{Failed: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return sample{StartedAt: started, Failed: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sample{StartedAt: started, Failed: true, StatusCode: resp.StatusCode}
	}

	var firstChunkAt time.Time
	var prevChunkAt time.Time
	var itls []time.Duration

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			// Unexpected read error mid-stream → mark failed but report partial timing.
			return sample{StartedAt: started, TTFT: timeSince(started, firstChunkAt), ITLs: itls, ResponseTime: time.Since(started), Failed: true}
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		now := time.Now()
		if firstChunkAt.IsZero() {
			firstChunkAt = now
		} else {
			itls = append(itls, now.Sub(prevChunkAt))
		}
		prevChunkAt = now
	}

	return sample{
		StartedAt:    started,
		TTFT:         timeSince(started, firstChunkAt),
		ITLs:         itls,
		ResponseTime: time.Since(started),
		StatusCode:   resp.StatusCode,
	}
}

// timeSince returns t.Sub(start), or 0 if t is zero.
func timeSince(start, t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return t.Sub(start)
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -run TestRunOneRequest -v`
Expected: 2 tests pass.

- [ ] **Step 5: Commit**

```bash
git add vllm-server-evaluator/generator.go vllm-server-evaluator/generator_test.go
git commit -m "feat(vllm-server-evaluator): streaming /v1/completions client

Sends one synthetic streaming request to vLLM and extracts TTFT,
per-chunk ITL, and end-to-end response time from the SSE stream.

Refs #7"
```

### Task 4.5: Poisson scheduler

**Files:**
- Modify: `vllm-server-evaluator/generator.go`
- Modify: `vllm-server-evaluator/generator_test.go`

- [ ] **Step 1: Append failing test for `runWindow`**

Append to `vllm-server-evaluator/generator_test.go`:

```go
func TestRunWindow_CollectsExpectedSampleCount(t *testing.T) {
	srv := fakeVLLMServer(t, 2, 10*time.Millisecond, 20*time.Millisecond)
	defer srv.Close()

	wp := windowParams{
		BaseURL:       srv.URL,
		Model:         "m",
		Spec:          requestSpec{InputTokens: 4, OutputTokens: 2, IgnoreEOS: true},
		RPS:           20.0,
		WarmupSec:     0,
		MinWindowSec:  1,
		MaxWindowSec:  3,
		TargetSamples: 10,
		Concurrency:   8,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := runWindow(ctx, wp)
	if err != nil {
		t.Fatalf("runWindow: %v", err)
	}
	if len(res.Samples) < 10 {
		t.Errorf("samples = %d, want >= 10 (target reached)", len(res.Samples))
	}
}

func TestRunWindow_CapsAtMaxWindow(t *testing.T) {
	srv := fakeVLLMServer(t, 2, 10*time.Millisecond, 20*time.Millisecond)
	defer srv.Close()

	wp := windowParams{
		BaseURL:       srv.URL,
		Model:         "m",
		Spec:          requestSpec{InputTokens: 4, OutputTokens: 2, IgnoreEOS: true},
		RPS:           1.0, // very slow
		WarmupSec:     0,
		MinWindowSec:  1,
		MaxWindowSec:  2,    // 1.0 RPS over 2s = ~2 samples max — far below target
		TargetSamples: 1000,
		Concurrency:   2,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := runWindow(ctx, wp)
	if err != nil {
		t.Fatalf("runWindow: %v", err)
	}
	elapsed := res.WindowEnd.Sub(res.WindowStart)
	if elapsed < 1*time.Second || elapsed > 3*time.Second {
		t.Errorf("window length = %v, want ~2s (capped)", elapsed)
	}
}
```

- [ ] **Step 2: Run — confirm undefined**

Run: `go test ./vllm-server-evaluator/...`
Expected: `runWindow`, `windowParams` undefined.

- [ ] **Step 3: Append `runWindow` to `generator.go`**

Append to `vllm-server-evaluator/generator.go`:

```go
import (
	"fmt"
	"math"
	"math/rand"
	"sync"
)

// windowParams configures one measurement window.
type windowParams struct {
	BaseURL       string
	Model         string
	Spec          requestSpec
	RPS           float64
	WarmupSec     int
	MinWindowSec  int
	MaxWindowSec  int
	TargetSamples int
	Concurrency   int
	Seed          int64
}

// runWindow drives a Poisson stream of requests at wp.RPS for
// max(MinWindowSec, TargetSamples/RPS) seconds, capped at MaxWindowSec.
// Samples started during the warmup prefix are discarded from results.
//
// Concurrency limits the number of simultaneous in-flight requests; arrivals
// that would exceed it are simply dropped from this driver (mimicking real
// load that vLLM would queue itself — the per-request sample includes vLLM's
// own queue time).
func runWindow(ctx context.Context, wp windowParams) (*windowResult, error) {
	if wp.RPS <= 0 {
		return nil, fmt.Errorf("non-positive RPS: %v", wp.RPS)
	}
	if wp.Concurrency <= 0 {
		wp.Concurrency = 64
	}
	if wp.Seed == 0 {
		wp.Seed = time.Now().UnixNano()
	}

	rng := rand.New(rand.NewSource(wp.Seed))

	// Compute window length.
	target := float64(wp.TargetSamples) / wp.RPS
	wantSec := math.Max(float64(wp.MinWindowSec), target)
	if wantSec > float64(wp.MaxWindowSec) {
		wantSec = float64(wp.MaxWindowSec)
	}
	totalSec := float64(wp.WarmupSec) + wantSec
	deadline := time.Now().Add(time.Duration(totalSec * float64(time.Second)))
	windowStart := time.Now().Add(time.Duration(wp.WarmupSec) * time.Second)

	sem := make(chan struct{}, wp.Concurrency)
	var mu sync.Mutex
	var samples []sample
	var warmup int
	var wg sync.WaitGroup

	// Poisson interarrival: exponential with mean 1/RPS.
	for {
		gap := time.Duration(rng.ExpFloat64() / wp.RPS * float64(time.Second))
		select {
		case <-time.After(gap):
		case <-ctx.Done():
			wg.Wait()
			return &windowResult{Samples: samples, WindowStart: windowStart, WindowEnd: time.Now(), WarmupSamples: warmup}, ctx.Err()
		}
		if time.Now().After(deadline) {
			break
		}

		// Try to acquire concurrency slot; drop if full.
		select {
		case sem <- struct{}{}:
		default:
			continue
		}

		startedAt := time.Now()
		isWarmup := startedAt.Before(windowStart)
		seed := rng.Int63()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s := runOneRequest(ctx, wp.BaseURL, wp.Model, wp.Spec, seed)
			s.StartedAt = startedAt
			mu.Lock()
			defer mu.Unlock()
			if isWarmup {
				warmup++
				return
			}
			samples = append(samples, s)
		}()
	}
	wg.Wait()

	return &windowResult{
		Samples:       samples,
		WindowStart:   windowStart,
		WindowEnd:     time.Now(),
		WarmupSamples: warmup,
	}, nil
}
```

Merge the new `import` block into the existing one (don't leave two).

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -run TestRunWindow -v`
Expected: 2 tests pass.

- [ ] **Step 5: Run all tests**

Run: `go test ./vllm-server-evaluator/... -v`
Expected: all tests pass.

- [ ] **Step 6: Commit**

```bash
git add vllm-server-evaluator/generator.go vllm-server-evaluator/generator_test.go
git commit -m "feat(vllm-server-evaluator): Poisson scheduler over measurement window

Computes window length = max(min, target/rps), capped by max. Drops
arrivals that would exceed concurrency; warmup samples discarded.

Refs #7"
```

### Task 4.6: Open PR 4

- [ ] **Step 1: Push and open PR**

```bash
git push origin feat/vllm-server-evaluator

gh pr create --title "feat(vllm-server-evaluator): PR 4/6 - generator and metrics scrape" --body "$(cat <<'EOF'
## Summary

PR 4 of 6. Adds the load-driving and metrics-collection logic.

- `metrics.go` — minimal Prometheus text-format parser for queue/inference time histograms (no external dep)
- `prompt.go` — randomized synthetic token-id prompts to avoid prefix-cache hits
- `generator.go` — single-request streaming SSE driver + Poisson-arrivals window
- Tests use `httptest.Server` to fake vLLM completions and metrics endpoints

This PR does **not** wire into `/solve` yet. PR 5 ties everything together.

## Test plan

- [x] `go test ./vllm-server-evaluator/...` passes (~12 tests)
- [x] `go build ./...` succeeds

Refs #7
EOF
)"
```

---

## PR 5: Saturation detection + /solve handler

### Task 5.1: Saturation detection

**Files:**
- Create: `vllm-server-evaluator/saturation.go`
- Create: `vllm-server-evaluator/saturation_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"testing"
	"time"
)

func mkSamples(ttfts []time.Duration) []sample {
	out := make([]sample, len(ttfts))
	for i, t := range ttfts {
		out[i] = sample{TTFT: t}
	}
	return out
}

func TestDetectSaturation_TTFTTrend_Triggers(t *testing.T) {
	// Linear ramp from 100ms to 200ms (>50% growth).
	ttfts := []time.Duration{
		100 * time.Millisecond, 110 * time.Millisecond, 130 * time.Millisecond,
		150 * time.Millisecond, 175 * time.Millisecond, 200 * time.Millisecond,
	}
	res := windowResult{Samples: mkSamples(ttfts)}
	got := detectSaturation(res, 0)
	if got != "overloaded" {
		t.Errorf("got %q, want overloaded (TTFT trend)", got)
	}
}

func TestDetectSaturation_TTFTTrend_StableNoTrigger(t *testing.T) {
	ttfts := []time.Duration{
		100 * time.Millisecond, 105 * time.Millisecond, 102 * time.Millisecond,
		98 * time.Millisecond, 103 * time.Millisecond, 100 * time.Millisecond,
	}
	res := windowResult{Samples: mkSamples(ttfts)}
	if got := detectSaturation(res, 0); got != "" {
		t.Errorf("got %q, want empty (stable TTFT)", got)
	}
}

func TestDetectSaturation_QueueDominance(t *testing.T) {
	res := windowResult{
		Samples: mkSamples([]time.Duration{100 * time.Millisecond, 100 * time.Millisecond}),
		ScrapeAtStart: metricsScrape{QueueTimeSum: 0, QueueTimeCount: 0, InferTimeSum: 0, InferTimeCount: 0},
		ScrapeAtEnd:   metricsScrape{QueueTimeSum: 30, QueueTimeCount: 10, InferTimeSum: 10, InferTimeCount: 10},
	}
	if got := detectSaturation(res, 0); got != "overloaded" {
		t.Errorf("got %q, want overloaded (queue > infer)", got)
	}
}

func TestDetectSaturation_ErrorRate(t *testing.T) {
	samples := []sample{
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{TTFT: 100 * time.Millisecond},
		{Failed: true, StatusCode: 429},
	}
	res := windowResult{Samples: samples}
	if got := detectSaturation(res, 0); got != "overloaded" {
		t.Errorf("got %q, want overloaded (error rate)", got)
	}
}
```

- [ ] **Step 2: Run — confirm undefined**

Run: `go test ./vllm-server-evaluator/...`
Expected: `detectSaturation` undefined.

- [ ] **Step 3: Implement `saturation.go`**

```go
package main

import (
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

const (
	ttftTrendGrowthThreshold = 0.5  // >50% growth from start to end of window
	errorRateThreshold       = 0.05 // ≥5%
)

// detectSaturation evaluates three independent signals; if any triggers,
// returns evaluator.SaturationOverload. Otherwise returns "".
//
// minSamples avoids spurious trend detection on tiny windows.
func detectSaturation(r windowResult, minSamples int) string {
	// Signal 3: error rate (cheapest, do first).
	if rate := errorRate(r.Samples); rate >= errorRateThreshold {
		return evaluator.SaturationOverload
	}

	// Signal 1: TTFT trend.
	if len(r.Samples) >= max(2, minSamples) && ttftGrowth(r.Samples) > ttftTrendGrowthThreshold {
		return evaluator.SaturationOverload
	}

	// Signal 2: queue dominance from /metrics deltas.
	queueMean := windowDelta(r.ScrapeAtStart.QueueTimeSum, r.ScrapeAtEnd.QueueTimeSum,
		r.ScrapeAtStart.QueueTimeCount, r.ScrapeAtEnd.QueueTimeCount)
	inferMean := windowDelta(r.ScrapeAtStart.InferTimeSum, r.ScrapeAtEnd.InferTimeSum,
		r.ScrapeAtStart.InferTimeCount, r.ScrapeAtEnd.InferTimeCount)
	if inferMean > 0 && queueMean > inferMean {
		return evaluator.SaturationOverload
	}

	return evaluator.SaturationNone
}

func errorRate(samples []sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	failed := 0
	for _, s := range samples {
		if s.Failed {
			failed++
		}
	}
	return float64(failed) / float64(len(samples))
}

// ttftGrowth fits a line over the TTFT-by-index series and returns the
// fractional growth from start to end:  (end_y - start_y) / start_y.
// Uses a minimal least-squares slope.
func ttftGrowth(samples []sample) float64 {
	n := len(samples)
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumXX float64
	for i, s := range samples {
		x := float64(i)
		y := float64(s.TTFT.Milliseconds())
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	N := float64(n)
	denom := N*sumXX - sumX*sumX
	if denom == 0 {
		return 0
	}
	slope := (N*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / N
	startY := intercept
	endY := intercept + slope*float64(n-1)
	if startY <= 0 {
		return 0
	}
	return (endY - startY) / startY
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -run TestDetectSaturation -v`
Expected: 4 tests pass.

- [ ] **Step 5: Commit**

```bash
git add vllm-server-evaluator/saturation.go vllm-server-evaluator/saturation_test.go
git commit -m "feat(vllm-server-evaluator): saturation detection (3 signals)

TTFT trend (linear regression, >50% growth), queue dominance (mean
queue > mean infer from /metrics deltas), and error rate (>=5%).

Refs #7"
```

### Task 5.2: Aggregation helper

**Files:**
- Create: `vllm-server-evaluator/aggregate.go`
- Create: `vllm-server-evaluator/aggregate_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"testing"
	"time"
)

func TestAggregate_Means(t *testing.T) {
	res := windowResult{
		WindowStart: time.Now().Add(-10 * time.Second),
		WindowEnd:   time.Now(),
		Samples: []sample{
			{TTFT: 100 * time.Millisecond, ITLs: []time.Duration{20 * time.Millisecond, 30 * time.Millisecond}, ResponseTime: 200 * time.Millisecond},
			{TTFT: 120 * time.Millisecond, ITLs: []time.Duration{40 * time.Millisecond, 60 * time.Millisecond}, ResponseTime: 240 * time.Millisecond},
		},
		ScrapeAtStart: metricsScrape{QueueTimeSum: 0, QueueTimeCount: 0},
		ScrapeAtEnd:   metricsScrape{QueueTimeSum: 0.4, QueueTimeCount: 4},
	}
	ad := aggregate(res, 5.0 /*pd.RPS*/)

	// AvgTTFT = (100+120)/2 = 110
	if ad.AvgTTFT < 109 || ad.AvgTTFT > 111 {
		t.Errorf("AvgTTFT = %v, want ~110", ad.AvgTTFT)
	}
	// AvgITL: per-request means {25, 50}, overall mean 37.5
	if ad.AvgITL < 36 || ad.AvgITL > 39 {
		t.Errorf("AvgITL = %v, want ~37.5", ad.AvgITL)
	}
	// AvgRespTime = (200+240)/2 = 220
	if ad.AvgRespTime < 219 || ad.AvgRespTime > 221 {
		t.Errorf("AvgRespTime = %v, want ~220", ad.AvgRespTime)
	}
	// AvgWaitTime = 0.4/4 * 1000 = 100ms
	if ad.AvgWaitTime < 99 || ad.AvgWaitTime > 101 {
		t.Errorf("AvgWaitTime = %v, want ~100", ad.AvgWaitTime)
	}
	// MaxRPS always 0
	if ad.MaxRPS != 0 {
		t.Errorf("MaxRPS = %v, want 0", ad.MaxRPS)
	}
}

func TestAggregate_ThroughputCapAtRPS(t *testing.T) {
	// 100 samples in a 1-second window = 100 RPS measured. pd.RPS = 50.
	// Throughput must be capped at 50 (existing invariant).
	samples := make([]sample, 100)
	for i := range samples {
		samples[i] = sample{TTFT: 100 * time.Millisecond, ResponseTime: 100 * time.Millisecond}
	}
	res := windowResult{
		WindowStart: time.Now().Add(-1 * time.Second),
		WindowEnd:   time.Now(),
		Samples:     samples,
	}
	ad := aggregate(res, 50.0)
	if ad.Throughput > 50.0 {
		t.Errorf("Throughput = %v, want <= 50", ad.Throughput)
	}
}
```

- [ ] **Step 2: Run — confirm undefined**

Run: `go test ./vllm-server-evaluator/...`
Expected: `aggregate` undefined.

- [ ] **Step 3: Implement `aggregate.go`**

```go
package main

import (
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// aggregate folds a windowResult into AnalysisData, applying the
// throughput-capped-at-RPS invariant from the existing evaluators.
func aggregate(r windowResult, pdRPS float32) evaluator.AnalysisData {
	if len(r.Samples) == 0 {
		return evaluator.AnalysisData{}
	}

	var ttftSum, rtSum float64
	var itlMeanSum float64
	var itlMeanCount int
	completed := 0
	for _, s := range r.Samples {
		if s.Failed {
			continue
		}
		completed++
		ttftSum += float64(s.TTFT.Microseconds()) / 1000.0 // ms
		rtSum += float64(s.ResponseTime.Microseconds()) / 1000.0
		if len(s.ITLs) > 0 {
			var itlSum float64
			for _, itl := range s.ITLs {
				itlSum += float64(itl.Microseconds()) / 1000.0
			}
			itlMeanSum += itlSum / float64(len(s.ITLs))
			itlMeanCount++
		}
	}
	if completed == 0 {
		return evaluator.AnalysisData{}
	}

	avgTTFT := ttftSum / float64(completed)
	avgRT := rtSum / float64(completed)
	var avgITL float64
	if itlMeanCount > 0 {
		avgITL = itlMeanSum / float64(itlMeanCount)
	}

	windowSec := r.WindowEnd.Sub(r.WindowStart).Seconds()
	var throughput float64
	if windowSec > 0 {
		throughput = float64(completed) / windowSec
	}
	if float32(throughput) > pdRPS {
		throughput = float64(pdRPS)
	}

	queueMean := windowDelta(
		r.ScrapeAtStart.QueueTimeSum, r.ScrapeAtEnd.QueueTimeSum,
		r.ScrapeAtStart.QueueTimeCount, r.ScrapeAtEnd.QueueTimeCount,
	) * 1000.0 // sec → ms

	return evaluator.AnalysisData{
		Throughput:  float32(throughput),
		AvgRespTime: float32(avgRT),
		AvgWaitTime: float32(queueMean),
		AvgTTFT:     float32(avgTTFT),
		AvgITL:      float32(avgITL),
		MaxRPS:      0,
	}
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -run TestAggregate -v`
Expected: 2 tests pass.

- [ ] **Step 5: Commit**

```bash
git add vllm-server-evaluator/aggregate.go vllm-server-evaluator/aggregate_test.go
git commit -m "feat(vllm-server-evaluator): windowResult -> AnalysisData aggregator

Refs #7"
```

### Task 5.3: /solve handler integration

**Files:**
- Create: `vllm-server-evaluator/handler.go`
- Create: `vllm-server-evaluator/handler_test.go`
- Modify: `vllm-server-evaluator/main.go`

- [ ] **Step 1: Write the failing integration test**

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// fullFakeVLLM serves /v1/models, /v1/completions (streaming), /metrics.
type fullFakeVLLM struct {
	servedModel  string
	chunkCount   int
	chunkInter   time.Duration
	firstDelay   time.Duration
	queueSum     atomic.Pointer[float64]
	queueCount   atomic.Pointer[float64]
	inferSum     atomic.Pointer[float64]
	inferCount   atomic.Pointer[float64]
	completed    int64
}

func newFullFakeVLLM(t *testing.T, servedModel string, chunks int, inter, first time.Duration) (*httptest.Server, *fullFakeVLLM) {
	f := &fullFakeVLLM{servedModel: servedModel, chunkCount: chunks, chunkInter: inter, firstDelay: first}
	z := 0.0
	f.queueSum.Store(&z)
	f.queueCount.Store(&z)
	f.inferSum.Store(&z)
	f.inferCount.Store(&z)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": f.servedModel}}})
		case strings.HasSuffix(r.URL.Path, "/v1/completions"):
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			time.Sleep(f.firstDelay)
			for i := 0; i < f.chunkCount; i++ {
				fmt.Fprintf(w, "data: {\"choices\":[{\"text\":\"x\"}]}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				if i < f.chunkCount-1 {
					time.Sleep(f.chunkInter)
				}
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			atomic.AddInt64(&f.completed, 1)
		case r.URL.Path == "/metrics":
			fmt.Fprintf(w, "vllm:request_queue_time_seconds_sum %v\n", *f.queueSum.Load())
			fmt.Fprintf(w, "vllm:request_queue_time_seconds_count %v\n", *f.queueCount.Load())
			fmt.Fprintf(w, "vllm:request_inference_time_seconds_sum %v\n", *f.inferSum.Load())
			fmt.Fprintf(w, "vllm:request_inference_time_seconds_count %v\n", *f.inferCount.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func TestSolve_HappyPath(t *testing.T) {
	srv, _ := newFullFakeVLLM(t, "test-model", 5, 20*time.Millisecond, 50*time.Millisecond)

	r := gin.New()
	cfg := map[string]serverConfig{
		"H100|test-model": {
			VLLMServedModelName: "test-model",
			VLLMPort:            0,
			WarmupSec:           0,
			MinWindowSec:        1,
			MaxWindowSec:        3,
			TargetSamples:       10,
			MinSamples:          5,
			IgnoreEOS:           true,
			QueueTimeMetric:     "vllm:request_queue_time_seconds",
		},
	}
	state := &handlerState{
		Lookup: cfg,
		Pairing: &pairingState{
			VLLMPodIP: strings.TrimPrefix(srv.URL, "http://"),
			VLLMPort:  0,
		},
		BaseURLOverride: srv.URL, // test hook bypasses ip:port construction
	}
	r.POST("/solve", solveHandler(state))

	pd := evaluator.ProblemData{
		RPS:             20,
		AvgInputTokens:  16,
		AvgOutputTokens: 8,
		Accelerator:     "H100",
		Model:           "test-model",
	}
	body, _ := json.Marshal(pd)
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	var ad evaluator.AnalysisData
	if err := json.Unmarshal(rr.Body.Bytes(), &ad); err != nil {
		t.Fatal(err)
	}
	if ad.AvgTTFT == 0 || ad.AvgITL == 0 || ad.Throughput == 0 {
		t.Errorf("expected non-zero metrics, got %+v", ad)
	}
}

func TestSolve_PairingNotReady(t *testing.T) {
	r := gin.New()
	state := &handlerState{
		Lookup: map[string]serverConfig{},
	}
	r.POST("/solve", solveHandler(state))

	body, _ := json.Marshal(evaluator.ProblemData{Accelerator: "H100", Model: "m"})
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestSolve_UnknownModel(t *testing.T) {
	r := gin.New()
	state := &handlerState{
		Lookup:  map[string]serverConfig{},
		Pairing: &pairingState{VLLMPodIP: "127.0.0.1"},
	}
	r.POST("/solve", solveHandler(state))

	body, _ := json.Marshal(evaluator.ProblemData{Accelerator: "H100", Model: "missing"})
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestSolve_ServedModelMismatch(t *testing.T) {
	srv, _ := newFullFakeVLLM(t, "DIFFERENT-MODEL", 1, time.Millisecond, time.Millisecond)

	r := gin.New()
	cfg := map[string]serverConfig{
		"H100|test-model": {
			VLLMServedModelName: "test-model",
			MinWindowSec:        1, MaxWindowSec: 2, TargetSamples: 5, MinSamples: 1,
			IgnoreEOS: true, QueueTimeMetric: "vllm:request_queue_time_seconds",
		},
	}
	state := &handlerState{
		Lookup:          cfg,
		Pairing:         &pairingState{VLLMPodIP: strings.TrimPrefix(srv.URL, "http://")},
		BaseURLOverride: srv.URL,
	}
	r.POST("/solve", solveHandler(state))

	body, _ := json.Marshal(evaluator.ProblemData{
		RPS: 10, AvgInputTokens: 4, AvgOutputTokens: 2,
		Accelerator: "H100", Model: "test-model",
	})
	req := httptest.NewRequest(http.MethodPost, "/solve", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (served-model mismatch), body=%s", rr.Code, rr.Body.String())
	}
}

// silence unused imports in some build configs
var _ = context.Background
```

- [ ] **Step 2: Run — confirm undefined**

Run: `go test ./vllm-server-evaluator/...`
Expected: `solveHandler`, `handlerState` undefined.

- [ ] **Step 3: Implement `handler.go`**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/llm-inferno/server-sim/pkg/evaluator"
)

// handlerState is the shared state injected into solveHandler.
type handlerState struct {
	Lookup  map[string]serverConfig
	Pairing *pairingState

	// BaseURLOverride is a test hook. Production leaves this empty and the
	// handler constructs the URL from Pairing.VLLMPodIP:Pairing.VLLMPort.
	BaseURLOverride string

	// vllmMu serializes /solve calls per (vllm) endpoint. v1: only one paired
	// vLLM per evaluator, so a single mutex is enough.
	vllmMu sync.Mutex
}

func solveHandler(st *handlerState) gin.HandlerFunc {
	return func(c *gin.Context) {
		var pd evaluator.ProblemData
		if err := c.ShouldBindJSON(&pd); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
			return
		}

		if st.Pairing == nil || (st.BaseURLOverride == "" && st.Pairing.VLLMPodIP == "") {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vllm pairing not ready"})
			return
		}

		key := pd.Accelerator + "|" + pd.Model
		sc, ok := st.Lookup[key]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "unknown accelerator/model combination: " + pd.Accelerator + " / " + pd.Model,
			})
			return
		}

		baseURL := st.BaseURLOverride
		if baseURL == "" {
			baseURL = fmt.Sprintf("http://%s:%d", st.Pairing.VLLMPodIP, st.Pairing.VLLMPort)
		}

		st.vllmMu.Lock()
		defer st.vllmMu.Unlock()

		// 1. Validate served-model.
		if err := verifyServedModel(c.Request.Context(), baseURL, sc.VLLMServedModelName); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 2. Scrape /metrics at window start.
		startScrape, err := scrapeMetrics(c.Request.Context(), baseURL+"/metrics")
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "scrape metrics: " + err.Error()})
			return
		}

		// 3. Run the measurement window.
		wp := windowParams{
			BaseURL:       baseURL,
			Model:         sc.VLLMServedModelName,
			Spec:          requestSpec{InputTokens: int(pd.AvgInputTokens), OutputTokens: int(pd.AvgOutputTokens), IgnoreEOS: sc.IgnoreEOS},
			RPS:           float64(pd.RPS),
			WarmupSec:     sc.WarmupSec,
			MinWindowSec:  sc.MinWindowSec,
			MaxWindowSec:  sc.MaxWindowSec,
			TargetSamples: sc.TargetSamples,
			Concurrency:   pd.MaxConcurrency, // 0 → default 64 inside runWindow
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(sc.WarmupSec+sc.MaxWindowSec+10)*time.Second)
		defer cancel()
		res, err := runWindow(ctx, wp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "window: " + err.Error()})
			return
		}
		res.ScrapeAtStart = startScrape

		// 4. Scrape /metrics at window end.
		endScrape, err := scrapeMetrics(c.Request.Context(), baseURL+"/metrics")
		if err == nil {
			res.ScrapeAtEnd = endScrape
		}

		// 5. Insufficient-samples guard.
		completed := 0
		for _, s := range res.Samples {
			if !s.Failed {
				completed++
			}
		}
		if completed < sc.MinSamples {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": fmt.Sprintf("insufficient samples: need %d, got %d", sc.MinSamples, completed),
			})
			return
		}

		// 6. Aggregate + saturation.
		ad := aggregate(*res, pd.RPS)
		ad.Saturation = detectSaturation(*res, sc.MinSamples)

		c.IndentedJSON(http.StatusOK, ad)
	}
}

// verifyServedModel hits /v1/models and confirms the desired model is listed.
func verifyServedModel(ctx context.Context, baseURL, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("verify served model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("verify served model: /v1/models status %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode /v1/models: %w", err)
	}
	for _, m := range body.Data {
		if m.ID == want {
			return nil
		}
	}
	got := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		got = append(got, m.ID)
	}
	return fmt.Errorf("vllm serves %v, requested %s", got, want)
}
```

- [ ] **Step 4: Run — expect PASS**

Run: `go test ./vllm-server-evaluator/... -v`
Expected: all tests pass, including the four new `TestSolve_*` tests.

- [ ] **Step 5: Wire `solveHandler` into `main.go`**

Replace `vllm-server-evaluator/main.go`:

```go
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

	vllmPort := 8000
	for _, sc := range lookup {
		if sc.VLLMPort > 0 {
			vllmPort = sc.VLLMPort
		}
		break
	}

	state := &handlerState{Lookup: lookup}

	var pairing atomic.Pointer[pairingState]
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			ps, err := resolvePairing(ctx, vllmPort)
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
		panic(err)
	}
}
```

- [ ] **Step 6: Build + run all tests**

Run: `go build ./... && go test ./vllm-server-evaluator/... -v`
Expected: all tests pass.

- [ ] **Step 7: Commit**

```bash
git add vllm-server-evaluator/handler.go vllm-server-evaluator/handler_test.go vllm-server-evaluator/main.go
git commit -m "feat(vllm-server-evaluator): /solve handler integration

Wires together pairing, served-model validation, scrape, window,
aggregation, and saturation. Per-vLLM mutex serializes calls.

Refs #7"
```

### Task 5.4: Open PR 5

- [ ] **Step 1: Push and open PR**

```bash
git push origin feat/vllm-server-evaluator

gh pr create --title "feat(vllm-server-evaluator): PR 5/6 - /solve handler + saturation" --body "$(cat <<'EOF'
## Summary

PR 5 of 6. Wires everything together.

- `saturation.go` — three independent signals (TTFT trend, queue dominance, error rate)
- `aggregate.go` — windowResult → AnalysisData with throughput-cap-at-RPS invariant
- `handler.go` — full /solve flow: pairing check → served-model verify → scrape → window → aggregate → saturation
- Integration tests with full fake vLLM (`/v1/models`, `/v1/completions` SSE, `/metrics`)

## Test plan

- [x] `go test ./vllm-server-evaluator/...` passes (~22 tests)
- [x] `go build ./...` succeeds

Refs #7
EOF
)"
```

---

## PR 6: K8s manifests + docs

### Task 6.1: ConfigMap and pod manifest

**Files:**
- Create: `deploy/k8s/configmap-vllm-server.yaml`
- Create: `deploy/k8s/pod-vllm-server.yaml`

- [ ] **Step 1: Write `deploy/k8s/configmap-vllm-server.yaml`**

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: vllm-server-config
data:
  vllm-eval-config.json: |
    {
      "configs": [
        {
          "accelerator": "H100",
          "model": "ibm-granite/granite-3.1-8b-instruct",
          "vllmServedModelName": "granite",
          "vllmPort": 8000,
          "warmupSec": 5,
          "minWindowSec": 20,
          "maxWindowSec": 300,
          "targetSamples": 200,
          "minSamples": 50,
          "ignoreEOS": true,
          "queueTimeMetric": "vllm:request_queue_time_seconds"
        }
      ]
    }
```

- [ ] **Step 2: Write `deploy/k8s/pod-vllm-server.yaml`**

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: server-sim-vllm-server
  labels:
    app: server-sim
    evaluator: vllm-server
    inferno.server.pair-id: "MANUAL-DEV-PAIR-1"
    inferno.server.vllm-deployment: "vllm-granite-h100"
spec:
  serviceAccountName: vllm-server-evaluator
  containers:
    - name: server-sim
      image: server-sim:latest
      ports:
        - containerPort: 8080
      env:
        - name: EVALUATOR_URL
          value: "http://localhost:8081"
        - name: NOISE_ENABLED
          value: "false"
      resources:
        requests:
          memory: "128Mi"
          cpu: "100m"
        limits:
          memory: "256Mi"
          cpu: "500m"
    - name: evaluator
      image: evaluator:latest
      args: [ "vllm-server" ]
      ports:
        - containerPort: 8081
      env:
        - name: EVALUATOR_PORT
          value: "8081"
        - name: VLLM_EVAL_CONFIG_FILE
          value: "/app/config/vllm-eval-config.json"
        - name: POD_NAME
          valueFrom:
            fieldRef: { fieldPath: metadata.name }
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef: { fieldPath: metadata.namespace }
        - name: VLLM_NAMESPACE
          value: "default"
      volumeMounts:
        - name: vllm-server-config
          mountPath: /app/config
        - name: podinfo
          mountPath: /etc/podinfo
      resources:
        requests:
          memory: "256Mi"
          cpu: "200m"
        limits:
          memory: "512Mi"
          cpu: "1000m"
  volumes:
    - name: vllm-server-config
      configMap:
        name: vllm-server-config
    - name: podinfo
      downwardAPI:
        items:
          - path: pair-id
            fieldRef:
              fieldPath: "metadata.labels['inferno.server.pair-id']"
          - path: vllm-deployment
            fieldRef:
              fieldPath: "metadata.labels['inferno.server.vllm-deployment']"
```

- [ ] **Step 3: Commit**

```bash
git add deploy/k8s/configmap-vllm-server.yaml deploy/k8s/pod-vllm-server.yaml
git commit -m "deploy: ConfigMap and standalone pod manifest for vllm-server evaluator

Refs #7"
```

### Task 6.2: RBAC and reference vLLM Deployment

**Files:**
- Create: `deploy/k8s/rbac-vllm-server.yaml`
- Create: `deploy/k8s/deployment-vllm-template.yaml`

- [ ] **Step 1: Write `deploy/k8s/rbac-vllm-server.yaml`**

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: vllm-server-evaluator
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: vllm-server-evaluator
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: vllm-server-evaluator
subjects:
  - kind: ServiceAccount
    name: vllm-server-evaluator
roleRef:
  kind: Role
  name: vllm-server-evaluator
  apiGroup: rbac.authorization.k8s.io
```

- [ ] **Step 2: Write `deploy/k8s/deployment-vllm-template.yaml` (reference, not auto-applied)**

```yaml
# Reference template for the paired vLLM Deployment. The control-loop Actuator
# is responsible for creating/scaling Deployments matching this shape and for
# writing inferno.server.pair-id labels to pair pods 1:1 with the managed
# Deployment's pods. This file is documentation, not auto-applied.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-granite-h100
  labels:
    inferno.vllm.model: granite
    inferno.vllm.accelerator: H100
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vllm-granite-h100
  template:
    metadata:
      labels:
        app: vllm-granite-h100
        inferno.vllm.model: granite
        inferno.vllm.accelerator: H100
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8000"
        prometheus.io/path: "/metrics"
    spec:
      containers:
        - name: vllm
          image: vllm/vllm-openai:latest
          args:
            - "--model"
            - "ibm-granite/granite-3.1-8b-instruct"
            - "--served-model-name"
            - "granite"
            - "--max-model-len"
            - "4096"
            - "--port"
            - "8000"
          ports:
            - containerPort: 8000
          resources:
            limits:
              nvidia.com/gpu: 1
          readinessProbe:
            httpGet:
              path: /health
              port: 8000
            initialDelaySeconds: 60
            periodSeconds: 5
```

- [ ] **Step 3: Commit**

```bash
git add deploy/k8s/rbac-vllm-server.yaml deploy/k8s/deployment-vllm-template.yaml
git commit -m "deploy: RBAC and reference vLLM Deployment template

Refs #7"
```

### Task 6.3: Backend doc with Actuator contract

**Files:**
- Create: `docs/vllm-server-evaluator.md`

- [ ] **Step 1: Write `docs/vllm-server-evaluator.md`**

```markdown
# vllm-server Evaluator

The `vllm-server` evaluator implements the server-sim `POST /solve` API by
driving a real vLLM server with synthetic open-loop traffic and reporting
measured TTFT, ITL, response time, throughput, and queue time.

This document is the operational reference. The full design rationale lives
in [`docs/superpowers/specs/2026-05-28-vllm-server-evaluator-design.md`](superpowers/specs/2026-05-28-vllm-server-evaluator-design.md).

## How it differs from other evaluators

`dummy`, `queue-analysis`, and `blis` are pure functions of `ProblemData`.
`vllm-server` is not — every `/solve` call requires a real vLLM pod paired
1:1 with the evaluator's pod. Pairing is established by the **control-loop
Actuator** via labels and discovered by the evaluator via the K8s API.

## Configuration

`vllm-eval-config.json` is keyed by `accelerator|model`. See
`vllm-server-evaluator/vllm-eval-config.json` for an example.

| Field | Description |
|---|---|
| `accelerator`, `model` | lookup key (matches `ProblemData.Accelerator/Model`) |
| `vllmServedModelName` | value of vLLM's `--served-model-name`; defaults to `model` |
| `vllmPort` | vLLM container port |
| `warmupSec` | discarded prefix at start of each window |
| `minWindowSec`, `maxWindowSec` | window bounds (sec) |
| `targetSamples` | samples to aim for; if not reached by `maxWindowSec`, the window ends |
| `minSamples` | below this, `/solve` returns 500 with insufficient-samples |
| `ignoreEOS` | passed through to vLLM |
| `queueTimeMetric` | name of the queue-time histogram metric (used in scrape) |

## Environment variables

| Var | Default | Description |
|---|---|---|
| `EVALUATOR_PORT` | `8081` | listen port |
| `VLLM_EVAL_CONFIG_FILE` | `vllm-eval-config.json` | config path |
| `POD_NAME`, `POD_NAMESPACE` | from downward API | identifies the evaluator's own pod |
| `VLLM_NAMESPACE` | `POD_NAMESPACE` | namespace where paired vLLM Deployments live |

## Pairing — Actuator contract

The control-loop Actuator MUST satisfy these invariants:

1. **Replica lockstep.** For every managed Deployment with labels
   `inferno.server.evaluator=vllm-server` and
   `inferno.server.vllm-deployment=<vd>`, the Actuator scales Deployment `<vd>`
   to the same `spec.replicas` as the managed Deployment.

2. **Pairing labels.** After both Deployments are at the desired replica count,
   the Actuator labels exactly one managed pod and one vLLM pod with the same
   `inferno.server.pair-id=<uuid>`. Each pair-id is unique across both
   Deployments.

3. **Pairing reconciliation on pod replacement.** If any pod (either side) is
   replaced, the Actuator writes a fresh pair-id binding the new pod to its
   peer.

4. **Order.** vLLM Deployment scaled first; pair-id labels written only after
   the vLLM pod is `Ready`.

If any invariant is violated, the evaluator returns 503 from `/solve` and the
Collector excludes the pod that cycle. Graceful degradation; no data
corruption.

## Failure modes

| Condition | HTTP response |
|---|---|
| Unpaired or no Ready vLLM | 503 |
| Served-model mismatch | 400 |
| Insufficient samples within `maxWindowSec` | 500 |
| Per-request connection failures (≥5%) | 200 with `Saturation: overloaded` |
| `accelerator\|model` not in config | 400 |

## Saturation signals (any one triggers `Saturation: overloaded`)

1. TTFT linear-regression growth >50% across the window.
2. `vllm:request_queue_time_seconds` mean > `vllm:request_inference_time_seconds` mean.
3. ≥5% request error rate, or any 429 from vLLM.

## Local development

Use `deploy/k8s/pod-vllm-server.yaml` as a starting point. You'll need to:

1. Apply `deploy/k8s/rbac-vllm-server.yaml` to create the ServiceAccount.
2. Apply `deploy/k8s/configmap-vllm-server.yaml`.
3. Manually deploy a vLLM pod with `inferno.server.pair-id=MANUAL-DEV-PAIR-1`
   on the same node, in the namespace pointed to by `VLLM_NAMESPACE`.
4. Apply `deploy/k8s/pod-vllm-server.yaml`.
5. `kubectl port-forward server-sim-vllm-server 8080:8080` and POST to
   `/simulate` as you would with any other evaluator.
```

- [ ] **Step 2: Commit**

```bash
git add docs/vllm-server-evaluator.md
git commit -m "docs: vllm-server evaluator operational reference

Includes the four-invariant Actuator contract, env vars, failure
modes, and saturation signals.

Refs #7"
```

### Task 6.4: Update existing docs

**Files:**
- Modify: `docs/kubernetes-deployment.md`
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [ ] **Step 1: Add row to backend table in `docs/kubernetes-deployment.md`**

In the section "Evaluator Backends", change the table to add a row:

```markdown
| `["vllm-server"]` | `vllm-server-evaluator` | Drives a real paired vLLM server. Requires `vllm-eval-config.json` plus K8s RBAC; see [vllm-server-evaluator.md](./vllm-server-evaluator.md). |
```

In the "Pod Manifests" table, add:

```markdown
| `pod-vllm-server.yaml` | vllm-server | `vllm-server-config` |
```

- [ ] **Step 2: Add row to evaluator-backends table in `CLAUDE.md`**

In the "Evaluator backends" table:

```markdown
| `vllm-server-evaluator/` | Drives a real paired vLLM server (open-loop Poisson + streaming TTFT/ITL); pairing established by control-loop Actuator via labels |
```

Also add to "Configuration" section the new env vars:

```markdown
vllm-server-evaluator additional vars: `VLLM_EVAL_CONFIG_FILE`, `POD_NAME`, `POD_NAMESPACE`, `VLLM_NAMESPACE`, `EVALUATOR_PORT`.
```

- [ ] **Step 3: Add a phase 4 row to the README phases table**

In `README.md`, find the table starting at the line containing `| 1 | [Dummy]`. Add a fourth row immediately after the BLIS row:

```markdown
| 4 | [vllm-server](docs/vllm-server-evaluator.md) | Drives a real paired vLLM server (open-loop Poisson) |
```

Also find the `Run individual evaluators` code block (search for `go run ./dummy-evaluator`) and append:

```bash
# vllm-server (requires VLLM_EVAL_CONFIG_FILE and a paired vLLM pod)
VLLM_EVAL_CONFIG_FILE=vllm-server-evaluator/vllm-eval-config.json go run ./vllm-server-evaluator
```

- [ ] **Step 4: Verify build and tests still pass**

Run: `go build ./... && go test ./vllm-server-evaluator/... -v`
Expected: builds clean, all tests pass.

- [ ] **Step 5: Commit**

```bash
git add docs/kubernetes-deployment.md CLAUDE.md README.md
git commit -m "docs: register vllm-server backend in existing tables

Refs #7"
```

### Task 6.5: Open PR 6 (final)

- [ ] **Step 1: Push and open PR**

```bash
git push origin feat/vllm-server-evaluator

gh pr create --title "feat(vllm-server-evaluator): PR 6/6 - K8s manifests + docs" --body "$(cat <<'EOF'
## Summary

PR 6 of 6 (final). Adds K8s manifests, RBAC, and operational documentation.

- `deploy/k8s/configmap-vllm-server.yaml`, `pod-vllm-server.yaml`, `rbac-vllm-server.yaml`
- `deploy/k8s/deployment-vllm-template.yaml` — reference vLLM Deployment template
- `docs/vllm-server-evaluator.md` — operational reference incl. **four-invariant Actuator contract**
- Updates to `docs/kubernetes-deployment.md`, `CLAUDE.md`, `README.md`

After this PR merges, the control-loop side of the work can begin in
parallel. The Actuator extension and managed-Deployment template updates
are tracked in their own issue/PRs.

Closes #7

## Test plan

- [x] `kubectl apply --dry-run=client -f deploy/k8s/configmap-vllm-server.yaml -f deploy/k8s/pod-vllm-server.yaml -f deploy/k8s/rbac-vllm-server.yaml` succeeds
- [x] All previous test suites still pass

EOF
)"
```

---

## After all 6 PRs merge

- [ ] Hand off to control-loop work — open a corresponding issue in `llm-inferno/control-loop` referencing this issue and the spec, listing the four Actuator invariants.
- [ ] Update issue #7 status (close after PR 6 merges).
