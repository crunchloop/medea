package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/crunchloop/medea/gen/medea/v1"
)

func openTemp(t *testing.T) *BoltStore {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func cluster(name, k8s string) *pb.Cluster {
	return &pb.Cluster{Name: name, Desired: &pb.ClusterDesired{KubernetesVersion: k8s, TalosVersion: "v1.13.5"}}
}

func TestRoundTripAndRevisionBump(t *testing.T) {
	s := openTemp(t)

	rev, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if rev != 1 {
		t.Fatalf("first write revision = %d, want 1", rev)
	}

	got, gotRev, err := s.GetCluster("home")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GetDesired().GetKubernetesVersion() != "v1.36.1" {
		t.Fatalf("k8s = %q", got.GetDesired().GetKubernetesVersion())
	}
	if gotRev != 1 || got.Revision != 1 {
		t.Fatalf("revision mismatch: gotRev=%d record=%d", gotRev, got.Revision)
	}

	// A second, unrelated write bumps the global counter monotonically.
	rev2, err := s.PutClusterDesired(&pb.Cluster{Name: "other", Desired: &pb.ClusterDesired{}}, 0)
	if err != nil {
		t.Fatalf("put2: %v", err)
	}
	if rev2 != 2 {
		t.Fatalf("second write revision = %d, want 2", rev2)
	}
}

func TestCAS(t *testing.T) {
	s := openTemp(t)
	rev, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0)
	if err != nil {
		t.Fatal(err)
	}

	// Stale expected revision is rejected.
	if _, err := s.PutClusterDesired(cluster("home", "v1.36.2"), 0); err != ErrConflict {
		t.Fatalf("stale CAS err = %v, want ErrConflict", err)
	}

	// Current expected revision succeeds and advances the record revision.
	newRev, err := s.PutClusterDesired(cluster("home", "v1.36.2"), rev)
	if err != nil {
		t.Fatalf("CAS with current rev: %v", err)
	}
	if newRev <= rev {
		t.Fatalf("revision did not advance: %d -> %d", rev, newRev)
	}
	got, _, _ := s.GetCluster("home")
	if got.GetDesired().GetKubernetesVersion() != "v1.36.2" {
		t.Fatalf("value not updated: %q", got.GetDesired().GetKubernetesVersion())
	}

	// Creating an existing key with expected=0 conflicts.
	if _, err := s.PutClusterDesired(cluster("home", "x"), 0); err != ErrConflict {
		t.Fatalf("create-existing err = %v, want ErrConflict", err)
	}
}

func TestObservedNotPersistedButMerged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "medea.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0); err != nil {
		t.Fatal(err)
	}
	s.SetClusterObserved("home", &pb.ClusterObserved{KubernetesVersion: "v1.36.1", ControlPlaneReady: true})

	// Observed is merged into reads while the process lives.
	got, _, _ := s.GetCluster("home")
	if !got.GetObserved().GetControlPlaneReady() {
		t.Fatalf("observed not merged on read")
	}
	s.Close()

	// After reopen, observed is gone (rebuildable cache, never persisted).
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	got2, _, _ := s2.GetCluster("home")
	if got2 == nil {
		t.Fatal("desired lost across reopen")
	}
	if got2.GetObserved() != nil {
		t.Fatalf("observed survived reopen: %+v", got2.GetObserved())
	}
	if got2.GetDesired().GetKubernetesVersion() != "v1.36.1" {
		t.Fatalf("desired corrupted across reopen")
	}
}

func TestCrashRecoveryResumesRevisionAndRollout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "medea.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0); err != nil {
		t.Fatal(err)
	}
	// A mid-flight rollout record must survive a restart (resume-safety).
	if err := s.PutMachineRollout(&pb.MachineRollout{
		Cluster: "home", Addr: "10.0.0.12",
		State: pb.RolloutState_ROLLOUT_STATE_UPGRADING, TargetTalosVersion: "v1.13.6",
	}); err != nil {
		t.Fatal(err)
	}
	revBefore := s.lastRev
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })

	if s2.lastRev != revBefore {
		t.Fatalf("revision not recovered: got %d, want %d", s2.lastRev, revBefore)
	}
	r, err := s2.GetMachineRollout("home", "10.0.0.12")
	if err != nil || r == nil {
		t.Fatalf("rollout record lost: %v", err)
	}
	if r.GetState() != pb.RolloutState_ROLLOUT_STATE_UPGRADING {
		t.Fatalf("rollout state lost: %v", r.GetState())
	}
	// New writes continue from the recovered revision, not from 1.
	next, err := s2.PutClusterDesired(cluster("home2", "v1.36.1"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if next != revBefore+1 {
		t.Fatalf("post-recovery revision = %d, want %d", next, revBefore+1)
	}
}

func TestWatchSnapshotThenLive(t *testing.T) {
	s := openTemp(t)

	// Pre-existing records (revisions 1 and 2) form the snapshot.
	if _, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNodePoolDesired(&pb.NodePool{Cluster: "home", Name: "workers", Role: pb.Role_ROLE_WORKER}, 0); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Watch(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot: two events, in revision order.
	got1 := recv(t, ch)
	got2 := recv(t, ch)
	if got1.Revision != 1 || got2.Revision != 2 {
		t.Fatalf("snapshot order wrong: %d, %d", got1.Revision, got2.Revision)
	}

	// Live: a new write arrives after the snapshot, exactly once.
	if err := s.PutClusterRollout(rollout("home")); err != nil {
		t.Fatal(err)
	}
	live := recv(t, ch)
	if live.Revision != 3 || live.Kind != KindClusterRollout {
		t.Fatalf("live event wrong: rev=%d kind=%s", live.Revision, live.Kind)
	}

	// No duplicate of the snapshot revisions on the live stream.
	select {
	case extra := <-ch:
		t.Fatalf("unexpected extra event: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchSinceSkipsOld(t *testing.T) {
	s := openTemp(t)
	if _, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0); err != nil { // rev 1
		t.Fatal(err)
	}
	if _, err := s.PutClusterDesired(cluster("two", "v1.36.1"), 0); err != nil { // rev 2
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Watch(ctx, 1) // only want > rev 1
	if err != nil {
		t.Fatal(err)
	}
	ev := recv(t, ch)
	if ev.Revision != 2 {
		t.Fatalf("since=1 gave revision %d, want 2", ev.Revision)
	}
}

func TestExportImportRoundTripNoCredsNoObserved(t *testing.T) {
	s := openTemp(t)
	if _, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNodePoolDesired(&pb.NodePool{Cluster: "home", Name: "workers", Role: pb.Role_ROLE_WORKER, Members: []string{"10.0.0.12"}}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutMachineDesired(&pb.Machine{Cluster: "home", Pool: "workers", TalosEndpoint: "10.0.0.12", Role: pb.Role_ROLE_WORKER}, 0); err != nil {
		t.Fatal(err)
	}
	s.SetClusterObserved("home", &pb.ClusterObserved{KubernetesVersion: "v1.36.1"})

	var buf bytes.Buffer
	if err := s.Export(&buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	// Export must not leak observed/runtime state. (No credentials are ever in
	// the store, so this also documents the safety property of datastore.md §9.)
	if bytes.Contains(buf.Bytes(), []byte("observed")) {
		t.Fatalf("export contains observed state:\n%s", buf.String())
	}

	// Import into a fresh store reproduces desired state.
	s2 := openTemp(t)
	if err := s2.Import(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, _, err := s2.GetCluster("home")
	if err != nil || got == nil {
		t.Fatalf("imported cluster missing: %v", err)
	}
	nps, _ := s2.ListNodePools("home")
	if len(nps) != 1 || nps[0].GetMembers()[0] != "10.0.0.12" {
		t.Fatalf("imported nodepool wrong: %+v", nps)
	}
	ms, _ := s2.ListMachines("home", "workers")
	if len(ms) != 1 || ms[0].GetTalosEndpoint() != "10.0.0.12" {
		t.Fatalf("imported machine wrong: %+v", ms)
	}
}

func TestHostRoundTripCASListDelete(t *testing.T) {
	s := openTemp(t)
	h := &pb.Host{Cluster: "home", Mac: "aa:bb", Pool: "workers", Role: pb.Role_ROLE_WORKER,
		State: pb.HostState_HOST_STATE_REGISTERED, Labels: map[string]string{"role": "worker"}}
	rev, err := s.PutHostDesired(h, 0)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.PutHostDesired(h, 0); err != ErrConflict {
		t.Fatalf("stale CAS err = %v, want ErrConflict", err)
	}
	got, grev, err := s.GetHost("home", "aa:bb")
	if err != nil || got == nil || grev != rev || got.GetPool() != "workers" || got.GetLabels()["role"] != "worker" {
		t.Fatalf("get host wrong: %+v rev=%d err=%v", got, grev, err)
	}
	if _, err := s.PutHostDesired(&pb.Host{Cluster: "home", Mac: "cc:dd", Pool: "controlplane", Role: pb.Role_ROLE_CONTROLPLANE}, 0); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListHosts("home", ""); len(all) != 2 {
		t.Fatalf("list all = %d, want 2", len(all))
	}
	if workers, _ := s.ListHosts("home", "workers"); len(workers) != 1 || workers[0].GetMac() != "aa:bb" {
		t.Fatalf("list workers wrong: %+v", workers)
	}
	if err := s.DeleteHost("home", "aa:bb"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetHost("home", "aa:bb"); got != nil {
		t.Fatalf("host still present after delete: %+v", got)
	}
}

func TestDeleteClusterRemovesAllRecordsAndIsScoped(t *testing.T) {
	s := openTemp(t)

	// Seed the target cluster across every per-cluster bucket, plus a second
	// cluster that must survive untouched.
	if _, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutNodePoolDesired(&pb.NodePool{Cluster: "home", Name: "workers"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutHostDesired(&pb.Host{Cluster: "home", Mac: "aa:bb", Pool: "controlplane",
		Role: pb.Role_ROLE_CONTROLPLANE, State: pb.HostState_HOST_STATE_REGISTERED}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.PutClusterBootstrap(&pb.ClusterBootstrap{Cluster: "home", CpMac: "aa:bb",
		CpEndpoint: "https://10.0.0.1:6443", CpIp: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutClusterDesired(cluster("keep", "v1.36.1"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutHostDesired(&pb.Host{Cluster: "keep", Mac: "cc:dd", Pool: "workers",
		Role: pb.Role_ROLE_WORKER, State: pb.HostState_HOST_STATE_REGISTERED}, 0); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteCluster("home"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Every "home" record is gone.
	if c, _, _ := s.GetCluster("home"); c != nil {
		t.Fatalf("cluster record survived delete: %+v", c)
	}
	if nps, _ := s.ListNodePools("home"); len(nps) != 0 {
		t.Fatalf("node pools survived delete: %+v", nps)
	}
	if hs, _ := s.ListHosts("home", ""); len(hs) != 0 {
		t.Fatalf("hosts survived delete: %+v", hs)
	}
	if cb, _ := s.GetClusterBootstrap("home"); cb != nil {
		t.Fatalf("bootstrap survived delete (would block re-create): %+v", cb)
	}

	// The other cluster is untouched.
	if c, _, _ := s.GetCluster("keep"); c == nil {
		t.Fatal("delete removed the wrong cluster (keep is gone)")
	}
	if hs, _ := s.ListHosts("keep", ""); len(hs) != 1 {
		t.Fatalf("delete touched another cluster's hosts: %+v", hs)
	}

	// Idempotent, and empty name is rejected.
	if err := s.DeleteCluster("home"); err != nil {
		t.Fatalf("second delete not idempotent: %v", err)
	}
	if err := s.DeleteCluster(""); err == nil {
		t.Fatal("expected error for empty cluster name")
	}
}

func TestExportImportIncludesHosts(t *testing.T) {
	s := openTemp(t)
	if _, err := s.PutClusterDesired(cluster("home", "v1.36.1"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutHostDesired(&pb.Host{Cluster: "home", Mac: "aa:bb", Pool: "workers",
		Role: pb.Role_ROLE_WORKER, State: pb.HostState_HOST_STATE_REGISTERED}, 0); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Export(&buf); err != nil {
		t.Fatal(err)
	}
	s2 := openTemp(t)
	if err := s2.Import(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	hs, _ := s2.ListHosts("home", "")
	if len(hs) != 1 || hs[0].GetMac() != "aa:bb" || hs[0].GetState() != pb.HostState_HOST_STATE_REGISTERED {
		t.Fatalf("imported host wrong: %+v", hs)
	}
}

func rollout(c string) *pb.ClusterRollout {
	return &pb.ClusterRollout{Cluster: c, Phase: pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_UPGRADING, TargetKubernetesVersion: "v1.36.2"}
}

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch event")
		return Event{}
	}
}

// compile-time check that BoltStore satisfies Store.
var _ Store = (*BoltStore)(nil)
