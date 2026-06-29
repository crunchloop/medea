package provision

import (
	"context"
	"testing"
	"time"

	pb "github.com/bilby91/medea/gen/medea/v1"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
)

// THE guard test: the executor never acts on a cluster that isn't
// provisioning-enabled, even with a replica-managed pool + an available host.
func TestExecutorSkipsDisabledCluster(t *testing.T) {
	st := provStore(t)
	seedProv(t, st, false, pb.HostState_HOST_STATE_REGISTERED) // NOT enabled
	p := &fakeProv{}
	built := false
	kubeFor := func(context.Context, *pb.Cluster) (KubeOps, func(), error) {
		built = true
		return cpOnly(), func() {}, nil
	}
	secretsFor := func(string) (*secrets.Bundle, error) { return testBundle(t), nil }

	e := NewExecutor(st, p, fakeResolver{id: "s"}, kubeFor, secretsFor, "factory.talos.dev", "/dev/sda", time.Minute)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if built {
		t.Fatal("executor built a kube client for a disabled cluster")
	}
	if len(p.staged) != 0 {
		t.Fatalf("staged on a disabled cluster: %v", p.staged)
	}
}

func TestExecutorRunsEnabledPool(t *testing.T) {
	st := provStore(t)
	seedProv(t, st, true, pb.HostState_HOST_STATE_REGISTERED)
	p := &fakeProv{}
	kubeFor := func(context.Context, *pb.Cluster) (KubeOps, func(), error) { return cpOnly(), func() {}, nil }
	secretsFor := func(string) (*secrets.Bundle, error) { return testBundle(t), nil }

	e := NewExecutor(st, p, fakeResolver{id: "schem"}, kubeFor, secretsFor, "factory.talos.dev", "/dev/sda", time.Minute)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.staged) != 1 || p.staged[0] != "aa:bb" {
		t.Fatalf("executor did not drive scale-out: staged=%v", p.staged)
	}
}
