// Package kube is Medea's client for a managed cluster's kube-apiserver. It is
// used as an outward client only — never from inside the cluster
// (design/talos-client.md §1, PRD §8). This file covers the read/seed subset
// plus cordon/uncordon; PDB-respecting drain lands with the M2 rollout reconciler.
package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NodeInfo is the cluster-truth view of a node used to seed the store.
type NodeInfo struct {
	Name           string
	InternalIP     string
	Role           string // "controlplane" | "worker"
	KubeletVersion string
	Ready          bool
}

// Client wraps a Kubernetes clientset.
type Client struct {
	cs kubernetes.Interface
}

// New builds a Client from kubeconfig bytes (resolved from the CredentialStore).
func New(kubeconfig []byte) (*Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{cs: cs}, nil
}

// NewWithClientset is for tests (fake clientset).
func NewWithClientset(cs kubernetes.Interface) *Client { return &Client{cs: cs} }

// ServerHost returns the kube-apiserver URL from a kubeconfig (for seeding the
// cluster's kube endpoint).
func ServerHost(kubeconfig []byte) (string, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return "", err
	}
	return cfg.Host, nil
}

// ListNodes returns the cluster-truth node list for seeding.
func (c *Client) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	nl, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]NodeInfo, 0, len(nl.Items))
	for i := range nl.Items {
		n := &nl.Items[i]
		out = append(out, NodeInfo{
			Name:           n.Name,
			InternalIP:     internalIP(n),
			Role:           role(n),
			KubeletVersion: n.Status.NodeInfo.KubeletVersion,
			Ready:          nodeReady(n),
		})
	}
	return out, nil
}

// NodeReady reports whether the node's Ready condition is true.
func (c *Client) NodeReady(ctx context.Context, name string) (bool, error) {
	n, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return nodeReady(n), nil
}

// KubeletVersion returns the node's reported kubelet version.
func (c *Client) KubeletVersion(ctx context.Context, name string) (string, error) {
	n, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return n.Status.NodeInfo.KubeletVersion, nil
}

// Cordon marks a node unschedulable.
func (c *Client) Cordon(ctx context.Context, name string) error {
	return c.setUnschedulable(ctx, name, true)
}

// Uncordon marks a node schedulable.
func (c *Client) Uncordon(ctx context.Context, name string) error {
	return c.setUnschedulable(ctx, name, false)
}

func (c *Client) setUnschedulable(ctx context.Context, name string, v bool) error {
	patch := []byte(`{"spec":{"unschedulable":false}}`)
	if v {
		patch = []byte(`{"spec":{"unschedulable":true}}`)
	}
	_, err := c.cs.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// drainPollInterval is how often Drain re-checks after an eviction round.
var drainPollInterval = 2 * time.Second

// Drain cordons node and evicts its pods, respecting PodDisruptionBudgets — the
// eviction API returns 429 when a PDB would be violated, and Drain simply retries
// such pods on the next round rather than forcing. DaemonSet, mirror (static),
// and terminal pods are left alone. On timeout it returns an error naming the
// pods that could not be evicted (rollout-controller.md §3: halt and surface the
// blocking pod; never --force).
//
// DESTRUCTIVE: evicts workloads. Invoked by the rollout reconciler.
func (c *Client) Drain(ctx context.Context, name string, timeout time.Duration) error {
	if err := c.Cordon(ctx, name); err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var remaining []string
	for {
		pods, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(dctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + name,
		})
		if err != nil {
			if dctx.Err() != nil {
				return fmt.Errorf("drain %s timed out; pods remaining: %s", name, strings.Join(remaining, ", "))
			}
			return err
		}

		remaining = remaining[:0]
		var toEvict []*corev1.Pod
		for i := range pods.Items {
			p := &pods.Items[i]
			if isEvictable(p) {
				toEvict = append(toEvict, p)
				remaining = append(remaining, p.Namespace+"/"+p.Name)
			}
		}
		if len(toEvict) == 0 {
			return nil
		}

		for _, p := range toEvict {
			ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}}
			err := c.cs.CoreV1().Pods(p.Namespace).EvictV1(dctx, ev)
			switch {
			case err == nil, apierrors.IsNotFound(err):
				// evicted, or already gone
			case apierrors.IsTooManyRequests(err):
				// PDB would be violated — leave it; retry next round
			default:
				return fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
			}
		}

		select {
		case <-dctx.Done():
			return fmt.Errorf("drain %s timed out; pods remaining: %s", name, strings.Join(remaining, ", "))
		case <-time.After(drainPollInterval):
		}
	}
}

// isEvictable reports whether a pod should be evicted during a drain. DaemonSet,
// mirror (static), terminal, and already-terminating pods are skipped.
func isEvictable(p *corev1.Pod) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return false
	}
	if p.DeletionTimestamp != nil {
		return false
	}
	if _, ok := p.Annotations[corev1.MirrorPodAnnotationKey]; ok {
		return false
	}
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return false
		}
	}
	return true
}

func nodeReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func internalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

func role(n *corev1.Node) string {
	if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return "controlplane"
	}
	return "worker"
}
