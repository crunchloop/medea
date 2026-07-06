//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/crunchloop/medea/internal/kube"
)

// TestDrainEvictsWorkload runs the (destructive) kube.Drain against a real
// cluster: it schedules a pod on the worker, drains the worker, and verifies the
// pod is actually evicted. UpgradeOS is intentionally NOT exercised here — the
// docker provisioner's upgrade is not the bare-metal A/B path, so testing it on
// docker would validate the wrong thing (it belongs on qemu/hardware).
func TestDrainEvictsWorkload(t *testing.T) {
	c := Start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(c.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}

	kc, err := kube.New(c.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var worker string
	for _, n := range nodes {
		if n.Role == "worker" {
			worker = n.Name
		}
	}
	if worker == "" {
		t.Fatal("no worker node")
	}

	// Schedule a single-replica workload (lands on the worker; the control plane
	// is tainted NoSchedule).
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "drain-test", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "drain-test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "drain-test"}},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr(int64(0)),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						RunAsUser:      ptr(int64(65535)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "pause",
						Image: "registry.k8s.io/pause:3.9",
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments("default").Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	// Wait for the pod to be Running on the worker; capture its name.
	var podName string
	if !waitFor(ctx, 3*time.Minute, func() bool {
		pods, err := cs.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: "app=drain-test"})
		if err != nil {
			return false
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Spec.NodeName == worker && p.Status.Phase == corev1.PodRunning {
				podName = p.Name
				return true
			}
		}
		return false
	}) {
		t.Fatal("workload pod never reached Running on the worker")
	}

	// Drain the worker — the pod must be evicted.
	if err := kc.Drain(ctx, worker, 2*time.Minute); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The originally-running pod is gone (evicted); the replacement can't land on
	// the cordoned worker.
	if !waitFor(ctx, 1*time.Minute, func() bool {
		_, err := cs.CoreV1().Pods("default").Get(ctx, podName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}) {
		t.Fatalf("pod %s was not evicted", podName)
	}
}

func waitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if cond() {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

func ptr[T any](v T) *T { return &v }
