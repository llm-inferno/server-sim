package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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

func makePod(name, namespace, pairID string, ready bool, ip string) *corev1.Pod {
	condStatus := corev1.ConditionFalse
	if ready {
		condStatus = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			// resolvePairedVLLM selects on pair-id AND the existence of the
			// inferno.vllm.model label, so the fixture must carry both.
			Labels: map[string]string{
				"inferno.server.pair-id": pairID,
				"inferno.vllm.model":     "test-model",
			},
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
