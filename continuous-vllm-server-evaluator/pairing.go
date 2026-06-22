package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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

// resolvePairedVLLM finds the vLLM pod paired with this evaluator by listing
// pods in the given namespace with label inferno.server.pair-id=<pairID>,
// filtering to those in PodRunning phase with PodReady=True. Expects exactly
// one match.
func resolvePairedVLLM(ctx context.Context, c kubernetes.Interface, namespace, pairID string) (string, error) {
	pods, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "inferno.server.pair-id=" + pairID + ",inferno.vllm.model",
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

// pairingState is the cached pairing info this evaluator uses to reach its vLLM.
type pairingState struct {
	PairID         string
	VLLMNamespace  string
	VLLMDeployment string
	VLLMPodIP      string // empty until resolved
	VLLMPort       int
}

const downwardLabelDir = "/etc/podinfo"

// resolvePairing reads the downward-API labels and looks up the paired vLLM
// pod using the provided K8s client. Returns a populated pairingState, or an
// error in unpaired/cold-start conditions (caller should treat as 503-pending).
func resolvePairing(ctx context.Context, client kubernetes.Interface, port int) (*pairingState, error) {
	if client == nil {
		return nil, fmt.Errorf("k8s client not available (not running in cluster)")
	}

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
