# Talos & Kubernetes clients

**Status:** Draft for review
**Date:** 2026-06-25

Scope: how Medea reaches the clusters it operates — the Talos machine-API
client wrapper (OS upgrade, etcd snapshot, health, version) and the quarantined
Kubernetes-upgrade path, plus the small kube-apiserver client (readiness,
cordon/drain). Implements PRD §13 #15 (no shelling — import Talos Go packages)
and §8.4. Blocks **M2** (rollout reconciler) and the M1 state-seeding step.

This doc commits to the *approach and the seams*, not to exact upstream symbol
names: per #15 the imported `upgrade-k8s` lives in Talos's main module and is
**version-coupled**, so the precise function/option types are pinned and
verified against a specific Talos release at implementation time (§7). Treat
symbol names below as indicative.

## 1. The seam: small interfaces, one heavy impl

The whole point of #15's "quarantine" is this: reconcilers depend on **small
Medea-owned interfaces**, and the heavy Talos imports live behind them in one
package. So the rollout reconciler is unit-testable with fakes
(rollout-controller.md §7) and never imports Talos directly.

```go
// internal/talos — consumed by the rollout reconciler.

type Client interface {
    // Version returns the running Talos version on a node.
    Version(ctx context.Context, node string) (string, error)
    // Healthy reports whether a node's Talos services are healthy.
    Healthy(ctx context.Context, node string) (bool, error)
    // UpgradeOS triggers an A/B OS upgrade to image; the node reboots.
    UpgradeOS(ctx context.Context, node, image string) error
    // EtcdSnapshot streams an etcd snapshot from a control-plane node to w.
    EtcdSnapshot(ctx context.Context, node string, w io.Writer) error
}

// K8sUpgrader is the single seam over Talos's main-module upgrade-k8s
// orchestration — the only place that imports the heavy dependency (#15, §8.4).
type K8sUpgrader interface {
    UpgradeK8s(ctx context.Context, from, to string) error
}
```

```go
// internal/kube — the kube-apiserver client (client-go), used as an outward
// client only (PRD §8: never in-cluster).

type Kube interface {
    NodeReady(ctx context.Context, name string) (bool, error)
    KubeletVersion(ctx context.Context, name string) (string, error)
    Cordon(ctx context.Context, name string) error
    Drain(ctx context.Context, name string, timeout time.Duration) error // PDB-respecting, no force
    Uncordon(ctx context.Context, name string) error
}
```

Design note — **two clients, two failure meanings.** During a control-plane
upgrade the kube-apiserver disappears (reboot); the rollout must treat a `Kube`
connection error as *park-and-retry*, not failure (rollout-controller.md §4).
The wrappers therefore surface a distinct "unreachable" error
(`errors.Is(err, ErrUnreachable)`) so the reconciler can tell "node is
mid-reboot" from "operation genuinely failed."

## 2. Connection & credentials

The Talos client is built from credentials resolved through the
`CredentialStore` (api-and-auth.md §5), never from anything in the bbolt store:

```
talosconfig := credStore.TalosConfig(cluster)     // client certs + CA
endpoints   := cluster.endpoints.talos            // control-plane IPs (API routing)
c := talosmachinery.New(ctx,
        WithConfigBytes(talosconfig),
        WithEndpoints(endpoints...))
```

- **Endpoints vs node targeting.** Talos distinguishes *endpoints* (control-plane
  nodes the API requests route through) from the *node* an operation targets.
  Per-call we set the target node (`client.WithNodes(ctx, node)`); `node` is the
  machine's Talos endpoint identity (datastore.md uses the address as identity).
- **Caching.** One machinery client per cluster, cached for the process
  lifetime; cheap to recreate if creds rotate. The kube client is a cached
  `client-go` clientset built from `credStore.KubeConfig(cluster)`.

## 3. OS upgrade — getting the image right

`UpgradeOS` wraps the machinery `Upgrade` RPC (A/B install + reboot). The hard
part is **not** the call — it's choosing the correct installer image.

**Decision: preserve the node's existing image identity, bump only the version.**
Talos installer images encode the node's **system extensions / Image Factory
schematic**. If Medea upgrades to a bare `installer:<version>` it silently drops
extensions. So Medea derives the target image from what the node is *currently*
running:

- Read the node's current installer image / schematic (from its machine config
  or Image Factory meta) via the machinery client.
- Substitute the target Talos version, keeping the schematic/extension set.
- Result: `factory.talos.dev/installer/<schematic>:<target>` (or the plain
  `installer:<target>` when no schematic is in use).

This cluster is **netboot/Image-Factory provisioned**, so the schematic case is
the expected one — getting this wrong is a real regression, hence it's a
first-class decision, not an afterthought. (Cross-ref: the eventual provisioning
plane owns schematics; until then Medea reads them off the live nodes.)

Reboot mode: default power-cycle into the new boot partition. No staged upgrades
in v1.

## 4. Kubernetes upgrade — the quarantined heavy import

`UpgradeK8s` wraps Talos's own `upgrade-k8s` orchestration (it sequences
control-plane static-pod manifests and rolls kubelets — not a single RPC, §8.4).
This is the **only** package importing Talos's main module.

- Indicative upstream: the orchestration in `github.com/siderolabs/talos/pkg/cluster`
  (+ its `kubernetes` upgrade subpackage), driven with a from→to version path.
  Exact symbol + options type pinned at impl time against the supported Talos
  release (§7).
- Medea constructs the upstream "cluster provider" from the same talosconfig +
  endpoints, calls the upgrade with `{from, to}`, and **monitors to completion**
  by polling kubelet versions via the `Kube` client (rollout-controller.md §2.2)
  — Talos drives the disruption, Medea observes convergence.
- Because this import is version-coupled, it is isolated in
  `internal/talos/k8supgrade` so the dependency (and any breaking upstream
  change) touches exactly one file.

## 5. etcd snapshot

`EtcdSnapshot` wraps the machinery etcd-snapshot stream (a control-plane-node
operation), copying the stream to `w`. Used by the rollout reconciler's
`snapshotBeforeControlPlane` gate (rollout-controller.md §3) and, later, by the
backup feature.

- v1: the caller (reconciler) streams to a durable file local to Medea and logs
  the location (rollout-controller.md §6 open question). The snapshot path stays
  generic so the deferred backup feature reuses it rather than a throwaway.
- Must complete (and be flushed to disk) *before* any control-plane mutation;
  failure aborts the rollout.

## 6. Health & version (feeding observed)

`Version` and `Healthy` are how the in-memory observed cache (datastore.md §2)
gets populated — both at boot (the refresh pass, datastore.md §7) and during a
rollout's wait-for-healthy:

- **Version:** machinery `Version` RPC → per-node Talos version.
- **Healthy:** node-level Talos service health (service list all-running, or the
  upstream cluster health-check helpers). Combined with `Kube.NodeReady` and
  `Kube.KubeletVersion`, this is the full "node converged" predicate the rollout
  waits on.
- **K8s version (cluster observed):** `Kube.KubeletVersion` across nodes /
  server version.

State seeding (M1) is just this read path run once over the live cluster: build
clients from `_out/{talosconfig,kubeconfig}`, enumerate nodes, populate
`Cluster`/`NodePool`/`Machine` desired (from current reality) and observed.

## 7. Version coupling & pinning (#15)

- The **`machinery` module** (`pkg/machinery`) is separately versioned for
  external consumers and stable — it covers OS upgrade, etcd snapshot, version,
  health. Most of `internal/talos` depends only on this.
- The **main-module `upgrade-k8s`** import (§4) couples Medea's build to a Talos
  release. Mitigations: isolate it in `internal/talos/k8supgrade`; pin a Talos
  version in `go.mod`; document a supported Talos version range; verify exact
  symbols/option types against that pin when the code lands.
- Implication for M2: write the `K8sUpgrader` impl against a pinned Talos and
  exercise it in the integration tier before trusting it on the Beelinks.

## 8. Package layout

```
internal/
├── talos/
│   ├── client.go          machinery wrapper: Version, Healthy, UpgradeOS, EtcdSnapshot
│   ├── image.go           installer-image / schematic derivation (§3)
│   └── k8supgrade/        the quarantined main-module upgrade-k8s import (§4)
└── kube/
    └── client.go          client-go wrapper: NodeReady, KubeletVersion, cordon/drain
```

## 9. Test plan

- **Reconciler units use fakes of `Client`/`K8sUpgrader`/`Kube`** (defined here)
  — this is what makes rollout-controller.md §7's unit tests possible without a
  real cluster.
- **Image derivation (unit):** given a node running a factory schematic image at
  vX, target vY yields the same schematic at vY; bare installer case handled.
- **`ErrUnreachable` mapping (unit):** transient gRPC/connection errors surface
  as unreachable (so the reconciler parks), genuine errors do not.
- **Integration (Talos-in-docker, `talosctl cluster create`):** real `Version`,
  real `EtcdSnapshot`, real `UpgradeK8s` from→to on a throwaway cluster; real
  PDB-respecting drain via the kube client. This tier is where the version-coupled
  `k8supgrade` import is validated.
