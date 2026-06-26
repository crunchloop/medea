package kube

import (
	"context"
	"testing"
	"time"

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

func TestIsEvictable(t *testing.T) {
	mkPod := func(mut func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if mut != nil {
			mut(p)
		}
		return p
	}
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"running app pod", mkPod(nil), true},
		{"daemonset pod", mkPod(func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "ds"}}
		}), false},
		{"mirror pod", mkPod(func(p *corev1.Pod) {
			p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
		}), false},
		{"succeeded", mkPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }), false},
		{"failed", mkPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }), false},
		{"terminating", mkPod(func(p *corev1.Pod) {
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}), false},
		{"replicaset-owned", mkPod(func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs"}}
		}), true},
	}
	for _, tc := range cases {
		if got := isEvictable(tc.pod); got != tc.want {
			t.Errorf("%s: isEvictable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDrainNoEvictablePods(t *testing.T) {
	// Node hosts only a DaemonSet pod -> nothing to evict; Drain returns quickly
	// and the node ends up cordoned. (Full eviction/PDB/timeout behavior is
	// covered by the integration tier; the fake clientset does not delete on
	// EvictV1, so the wait loop can't be exercised in a unit test.)
	dsPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ds-pod", Namespace: "kube-system",
			OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "cilium"}},
		},
		Spec:   corev1.PodSpec{NodeName: "w1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := fake.NewSimpleClientset(node("w1", "10.0.0.1", "v1.36.1", false, true), dsPod)
	c := NewWithClientset(cs)
	ctx := context.Background()

	if err := c.Drain(ctx, "w1", 5*time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("Drain did not cordon the node")
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
