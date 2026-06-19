package server

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleLabels = `app="vllm-qwen-14b-server"
inferno.server.load.rpm="300"
inferno.server.load.intokens="1024"
inferno.server.load.outtokens="512"
inferno.server.model="qwen_2_5_14b"
inferno.server.allocation.accelerator="H100"
inferno.server.allocation.maxbatchsize="32"
`

func TestReadLabelsAndConvert(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "labels"), []byte(sampleLabels), 0o644); err != nil {
		t.Fatal(err)
	}
	labels, err := ReadLabels(filepath.Join(dir, "labels"))
	if err != nil {
		t.Fatalf("ReadLabels: %v", err)
	}
	pd, ok := LabelsToProblemData(labels)
	if !ok {
		t.Fatal("LabelsToProblemData ok=false, want true")
	}
	if pd.RPS != 5 { // 300/60
		t.Fatalf("RPS = %v, want 5", pd.RPS)
	}
	if pd.AvgInputTokens != 1024 || pd.AvgOutputTokens != 512 {
		t.Fatalf("tokens = %v/%v", pd.AvgInputTokens, pd.AvgOutputTokens)
	}
	if pd.MaxConcurrency != 32 || pd.Model != "qwen_2_5_14b" || pd.Accelerator != "H100" {
		t.Fatalf("bad pd: %+v", pd)
	}
}

func TestLabelsToProblemDataMissingFieldNotReady(t *testing.T) {
	// rpm absent (pod not yet labelled by the load emulator) → not ready.
	labels := map[string]string{
		"inferno.server.model":                  "m",
		"inferno.server.allocation.accelerator": "H100",
	}
	if _, ok := LabelsToProblemData(labels); ok {
		t.Fatal("ok should be false when rpm is missing")
	}
}
