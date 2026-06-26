//go:build integration

package itest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/talos"
)

// TestEtcdSnapshot validates the (non-destructive) etcd snapshot against a real
// control-plane node. UpgradeOS and Drain are destructive even to the scratch
// cluster and are exercised by the rollout reconciler's integration test, where
// ordering is controlled.
func TestEtcdSnapshot(t *testing.T) {
	c := Start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	kc, err := kube.New(c.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cpIP string
	for _, n := range nodes {
		if n.Role == "controlplane" {
			cpIP = n.InternalIP
		}
	}
	if cpIP == "" {
		t.Fatal("no control-plane node")
	}

	tc, err := talos.New(ctx, c.Talosconfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	var buf bytes.Buffer
	if err := tc.EtcdSnapshot(ctx, cpIP, &buf); err != nil {
		t.Fatalf("EtcdSnapshot: %v", err)
	}
	// A real etcd snapshot (bbolt file) is well more than a few KB.
	if buf.Len() < 4096 {
		t.Fatalf("snapshot suspiciously small: %d bytes", buf.Len())
	}
}
