//go:build integration

package integration

import "testing"

// TestClusterSuite boots ONE scratch docker cluster and runs the read-only
// integration checks against it as parallel subtests. Standing up a Talos
// cluster is the dominant cost (minutes); the checks themselves are seconds — so
// sharing one cluster across the non-destructive tests collapses N boots into
// one.
//
// Only NON-DESTRUCTIVE checks belong here. The destructive ones keep their own
// cluster and stay separate top-level tests (which run sequentially, so their
// cluster never overlaps this one):
//   - TestDrainEvictsWorkload cordons/drains the worker.
//   - TestK8sUpgrade mutates the cluster's Kubernetes version.
func TestClusterSuite(t *testing.T) {
	c := Start(t)

	t.Run("Wrappers", func(t *testing.T) {
		t.Parallel()
		testWrappersAgainstRealCluster(t, c)
	})
	t.Run("CaptureSecrets", func(t *testing.T) {
		t.Parallel()
		testCaptureSecrets(t, c)
	})
	t.Run("EtcdSnapshot", func(t *testing.T) {
		t.Parallel()
		testEtcdSnapshot(t, c)
	})
}
