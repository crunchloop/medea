package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func node(name, ip, kubelet string, cp, ready bool) *corev1.Node {
	labels := map[string]string{}
	if cp {
		labels["node-role.kubernetes.io/control-plane"] = ""
	}
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: kubelet},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
		},
	}
}

func TestListNodes(t *testing.T) {
	cs := fake.NewSimpleClientset(
		node("cp", "10.0.0.10", "v1.36.1", true, true),
		node("w1", "10.0.0.11", "v1.36.1", false, true),
	)
	c := NewWithClientset(cs)

	nodes, err := c.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes", len(nodes))
	}
	byName := map[string]NodeInfo{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	if byName["cp"].Role != "controlplane" || byName["w1"].Role != "worker" {
		t.Fatalf("roles wrong: %+v", byName)
	}
	if byName["cp"].InternalIP != "10.0.0.10" || byName["cp"].KubeletVersion != "v1.36.1" || !byName["cp"].Ready {
		t.Fatalf("cp node info wrong: %+v", byName["cp"])
	}
}

func TestNodeReadyAndKubeletVersion(t *testing.T) {
	cs := fake.NewSimpleClientset(node("w1", "10.0.0.1", "v1.36.1", false, false))
	c := NewWithClientset(cs)
	ready, err := c.NodeReady(context.Background(), "w1")
	if err != nil || ready {
		t.Fatalf("NodeReady = %v, %v; want false", ready, err)
	}
	v, err := c.KubeletVersion(context.Background(), "w1")
	if err != nil || v != "v1.36.1" {
		t.Fatalf("KubeletVersion = %q, %v", v, err)
	}
}

func TestCordonUncordon(t *testing.T) {
	cs := fake.NewSimpleClientset(node("w1", "10.0.0.1", "v1.36.1", false, true))
	c := NewWithClientset(cs)
	ctx := context.Background()

	if err := c.Cordon(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("cordon did not set unschedulable")
	}
	if err := c.Uncordon(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	n, _ = cs.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Fatal("uncordon did not clear unschedulable")
	}
}
