// Package kube is Medea's client for a managed cluster's kube-apiserver. It is
// used as an outward client only — never from inside the cluster
// (design/talos-client.md §1, PRD §8). This file covers the read/seed subset
// plus cordon/uncordon; PDB-respecting drain lands with the M2 rollout reconciler.
package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
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
