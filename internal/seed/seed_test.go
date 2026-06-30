package seed

import (
	"path/filepath"
	"testing"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/kube"
	"github.com/crunchloop/medea/internal/store"
)

func TestApplySeedsClusterPoolsMachines(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	in := Inputs{
		Cluster:        "home",
		KubeEndpoint:   "https://10.0.0.10:6443",
		TalosEndpoints: []string{"10.0.0.10"},
		Nodes: []kube.NodeInfo{
			{Name: "cp", InternalIP: "10.0.0.10", Role: "controlplane", KubeletVersion: "v1.36.1", Ready: true},
			{Name: "w1", InternalIP: "10.0.0.11", Role: "worker", KubeletVersion: "v1.36.1", Ready: true},
			{Name: "w2", InternalIP: "10.0.0.12", Role: "worker", KubeletVersion: "v1.36.1", Ready: true},
		},
		TalosVersion: func(addr string) (string, error) { return "v1.13.5", nil },
	}

	res, err := Apply(st, in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Machines != 3 || len(res.Pools) != 2 {
		t.Fatalf("result: %+v", res)
	}

	// Cluster desired = current reality (so no rollout triggers).
	cl, _, _ := st.GetCluster("home")
	if cl.GetDesired().GetTalosVersion() != "v1.13.5" || cl.GetDesired().GetKubernetesVersion() != "v1.36.1" {
		t.Fatalf("cluster desired wrong: %+v", cl.GetDesired())
	}
	if cl.GetEndpoints().GetKube() != "https://10.0.0.10:6443" {
		t.Fatalf("kube endpoint wrong: %q", cl.GetEndpoints().GetKube())
	}

	// Pools by role.
	cp, _, _ := st.GetNodePool("home", "controlplane")
	if cp == nil || cp.GetRole() != pb.Role_ROLE_CONTROLPLANE || len(cp.GetMembers()) != 1 {
		t.Fatalf("controlplane pool wrong: %+v", cp)
	}
	w, _, _ := st.GetNodePool("home", "workers")
	if w == nil || w.GetRole() != pb.Role_ROLE_WORKER || len(w.GetMembers()) != 2 {
		t.Fatalf("workers pool wrong: %+v", w)
	}

	// Machines.
	ms, _ := st.ListMachines("home", "")
	if len(ms) != 3 {
		t.Fatalf("machines: %d", len(ms))
	}

	// Idempotent: a second Apply doesn't error (CAS against existing revisions).
	if _, err := Apply(st, in); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	ms2, _ := st.ListMachines("home", "")
	if len(ms2) != 3 {
		t.Fatalf("machines after re-seed: %d", len(ms2))
	}
}
