package provision

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/store"
)

const (
	pNotBootstrapped = pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_NOT_BOOTSTRAPPED
	pAwaitingInstall = pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_AWAITING_INSTALL
	pAwaitingHealthy = pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_AWAITING_HEALTHY
	pReady           = pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_READY
)

// --- fakes ---

type fakeProvisioner struct{ staged int }

func (f *fakeProvisioner) Stage(_ context.Context, _ string, _ Profile, _ []byte) error {
	f.staged++
	return nil
}
func (f *fakeProvisioner) Unstage(_ context.Context, _ string) error { return nil }

// fakeResolver is defined in reconciler_test.go (struct{ id string }).

type fakeCreds struct {
	secrets, talos, kube map[string][]byte
}

func newFakeCreds() *fakeCreds {
	return &fakeCreds{secrets: map[string][]byte{}, talos: map[string][]byte{}, kube: map[string][]byte{}}
}
func (f *fakeCreds) Secrets(c string) ([]byte, error) {
	if b, ok := f.secrets[c]; ok {
		return b, nil
	}
	return nil, errors.New("no secrets")
}
func (f *fakeCreds) PutSecrets(c string, b []byte) error { f.secrets[c] = b; return nil }
func (f *fakeCreds) TalosConfig(c string) ([]byte, error) {
	if b, ok := f.talos[c]; ok {
		return b, nil
	}
	return nil, errors.New("no talosconfig")
}
func (f *fakeCreds) Put(c string, talos, kube []byte) error {
	if talos != nil {
		f.talos[c] = talos
	}
	if kube != nil {
		f.kube[c] = kube
	}
	return nil
}

// fakeTalos is a shared instance whose readiness the test flips as the (fake)
// node comes up.
type fakeTalos struct {
	reachable  bool
	kubeReady  bool
	bootstraps int
}

func (f *fakeTalos) Version(_ context.Context, _ string) (string, error) {
	if !f.reachable {
		return "", errors.New("unreachable")
	}
	return "v1.13.5", nil
}
func (f *fakeTalos) Bootstrap(_ context.Context, _ string) error { f.bootstraps++; return nil }
func (f *fakeTalos) Kubeconfig(_ context.Context, _ string) ([]byte, error) {
	if !f.kubeReady {
		return nil, errors.New("apiserver not up")
	}
	return []byte("KUBECONFIG"), nil
}

func TestBootstrapReconcilerDrivesToReady(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	creds := newFakeCreds()
	prov := &fakeProvisioner{}
	ft := &fakeTalos{}
	r := NewBootstrapReconciler(st, prov, fakeResolver{id: "schematicABC"}, creds,
		func(_ []byte, _ string) (BootstrapTalos, func(), error) { return ft, func() {}, nil },
		"factory.talos.dev", 0)

	if err := st.PutClusterBootstrap(&pb.ClusterBootstrap{
		Cluster: "home", Phase: pNotBootstrapped,
		CpMac: "aa:bb:cc", CpEndpoint: "https://10.0.0.2:6443", CpIp: "10.0.0.2",
		TalosVersion: "v1.13.5", KubernetesVersion: "v1.36.1", InstallDisk: "/dev/sda",
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	phase := func() pb.ClusterBootstrapPhase {
		cb, _ := st.GetClusterBootstrap("home")
		return cb.GetPhase()
	}
	step := func() {
		if err := r.Reconcile(ctx, "home"); err != nil {
			t.Fatalf("reconcile at %v: %v", phase(), err)
		}
	}

	step() // NOT_BOOTSTRAPPED -> GENERATING_SECRETS
	step() // GENERATING_SECRETS -> STAGING (mint + store secrets)
	step() // STAGING -> AWAITING_INSTALL (render + stage + store talosconfig)

	// The node isn't reachable yet: reconciling parks (no phase change).
	step()
	step()
	if phase() != pAwaitingInstall {
		t.Fatalf("expected to park in AWAITING_INSTALL, got %v", phase())
	}

	ft.reachable = true
	step() // AWAITING_INSTALL -> BOOTSTRAPPING
	step() // BOOTSTRAPPING -> AWAITING_HEALTHY (etcd bootstrapped once)

	// apiserver not up yet: parks in AWAITING_HEALTHY.
	step()
	if phase() != pAwaitingHealthy {
		t.Fatalf("expected to park in AWAITING_HEALTHY, got %v", phase())
	}

	ft.kubeReady = true
	step() // AWAITING_HEALTHY -> READY (fetch kubeconfig, seed inventory)

	if phase() != pReady {
		t.Fatalf("phase = %v, want READY", phase())
	}
	// Terminal: further reconciles are no-ops and never re-bootstrap.
	step()
	step()

	if ft.bootstraps != 1 {
		t.Fatalf("Bootstrap called %d times, want exactly 1", ft.bootstraps)
	}
	if prov.staged != 1 {
		t.Fatalf("Stage called %d times, want exactly 1", prov.staged)
	}
	if len(creds.secrets["home"]) == 0 {
		t.Fatal("secrets were not generated")
	}
	if string(creds.kube["home"]) != "KUBECONFIG" {
		t.Fatalf("kubeconfig not stored: %q", creds.kube["home"])
	}
	// Inventory was seeded so Medea now operates the cluster.
	if cl, _, _ := st.GetCluster("home"); cl == nil {
		t.Fatal("cluster desired record not seeded")
	}
	if np, _, _ := st.GetNodePool("home", "controlplane"); np == nil {
		t.Fatal("control-plane nodepool not seeded")
	}
}

// Re-running GENERATING_SECRETS when a bundle already exists must not mint a new
// one (secrets-once — else it would be a different cluster).
func TestBootstrapReconcilerSecretsOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	creds := newFakeCreds()
	creds.secrets["home"] = []byte("EXISTING-BUNDLE") // pretend a prior run minted one
	r := NewBootstrapReconciler(st, &fakeProvisioner{}, fakeResolver{id: "schematicABC"}, creds,
		func(_ []byte, _ string) (BootstrapTalos, func(), error) { return &fakeTalos{}, func() {}, nil },
		"factory.talos.dev", 0)

	if err := st.PutClusterBootstrap(&pb.ClusterBootstrap{
		Cluster: "home",
		Phase:   pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_GENERATING_SECRETS,
		CpIp:    "10.0.0.2", CpEndpoint: "https://10.0.0.2:6443", KubernetesVersion: "v1.36.1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(context.Background(), "home"); err != nil {
		t.Fatal(err)
	}
	if string(creds.secrets["home"]) != "EXISTING-BUNDLE" {
		t.Fatal("existing secrets bundle was overwritten — not secrets-once")
	}
}
