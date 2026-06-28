# Aggregate: Cluster

**Context:** Cluster Inventory (supporting) · **Type:** aggregate root ·
**Status:** Implemented

`Cluster` is the top of the inventory: the registry record for one managed Talos
cluster. It holds the operator's *desired* cluster-wide versions, the connection
*endpoints*, and the two rollout gates the core domain reads — `mode` and
`rolloutsEnabled`. It is also where the desired-vs-observed split is most
visible.

- **Type:** `pb.Cluster` ([`gen/medea/v1`](../../gen/medea/v1))
- **Created by:** `seed.Apply` ([`internal/seed`](../../internal/seed))
- **Mutated by:** `server.SetClusterVersions`, `EnableRollouts`/`DisableRollouts`
  ([`internal/server`](../../internal/server))
- **Observed projected by:** `refresh.Refresher` ([`internal/refresh`](../../internal/refresh))

## Consistency boundary

- **One record per cluster name**, stored in `bDesired/clusters`
  (`store.PutClusterDesired`/`GetCluster`, key = the name).
- **Desired is persisted and written via CAS** (`expected Revision`). **Observed
  is in-memory only** (`store.SetClusterObserved`) and never written to bbolt —
  it is rebuilt on boot by refresh (`datastore.md` §2).
- **`ClusterRollout`** (the K8s-path phase) is a *separate* record in
  `bRollouts/clusters`, not part of this aggregate — see
  [`cluster-rollout.md`](cluster-rollout.md).

## Lifecycle

No formal state machine — it is a registry record with three independently
mutated facets:

- **identity + endpoints + cluster-wide desired** — written once by `seed`
  (desired = current reality), updated by `SetClusterVersions`.
- **`rolloutsEnabled`** — flipped only by `EnableRollouts`/`DisableRollouts`.
- **observed** (`kubernetesVersion`, `controlPlaneReady`) — overwritten each
  refresh pass; set to `controlPlaneReady=false` when the cluster is unreachable.

## Invariants

| # | Invariant | Enforced by | Why |
| --- | --- | --- | --- |
| I1 | **Desired is precious (persisted, CAS); observed is a rebuildable in-memory cache, never persisted.** | `store.PutClusterDesired` (CAS) vs `SetClusterObserved` (in-memory map) | Only desired exists solely in Medea; observed is always re-readable, so persisting it would add durability burden and a stale-truth risk (`datastore.md` §2). |
| I2 | **`rolloutsEnabled` defaults to `false` and is never set by seeding.** | `seed.Apply` (never writes the field) + `EnableRollouts`/`DisableRollouts` as the only mutators | The hard guard that keeps the live cluster un-rollout-able by accident (`rollout-safety.md` §3 #1). |
| I3 | **`mode` defaults to manual; `auto` is refused.** | unspecified == manual (proto default); `CreateRollout` rejects `CLUSTER_MODE_AUTO` | Drift-reconcile must never auto-mutate a cluster in v1 (`rollout-safety.md` §2). |
| I4 | **`SetClusterVersions` is a partial update** — omitted `talos`/`k8s` fields are left intact — with optional CAS via `expected_revision`. | `server.SetClusterVersions` (`rmw`: pointer fields, conditional revision check) | `--talos` and `--k8s` are independent; default LWW-at-intent fits a single operator, while `expected_revision` gives scripts a safety valve (`api-and-auth.md` §2). |
| I5 | **Cluster-wide desired versions are seeded from a control-plane node.** | `seed.Apply` (reads the control-plane node's Talos + kubelet versions; falls back to the first node) | The control-plane node defines the cluster's canonical versions. |
| I6 | **Credentials are referenced by cluster name only; no secret material lives in the record.** | `Cluster.endpoints` holds addresses; talosconfig/kubeconfig live in `creds.Store` keyed by name | Keeps the desired-state export safe to inspect/commit (`api-and-auth.md` §5, `datastore.md` §9). |

## Command surface

| Command (gRPC) | CLI | Effect |
| --- | --- | --- |
| `GetCluster` / `ListClusters` | `medea get clusters` | Read (returns desired + observed + endpoints + gates). |
| `SetClusterVersions` | `medea upgrade --k8s …` (cluster-wide) | Partial update of cluster desired versions (I4). |
| `EnableRollouts` / `DisableRollouts` | `medea cluster enable-rollouts\|disable-rollouts` | Flips `rolloutsEnabled` (I2). |

Creation is out-of-band: `medea seed` (run with the server stopped) bootstraps
the record from a live cluster.

## Event surface

- `cluster` — emitted on any desired write (versions, enable/disable). Observed
  changes do **not** emit events (observed is never a store write).

## Cross-context dependencies

- **Version Rollout** reads `rolloutsEnabled`, `mode`, and `endpoints.talos`
  (the gates and the Talos API routing) — this aggregate is the authorization
  context for the core domain.
- **Talos / Kubernetes Integration:** `endpoints` + the name-referenced
  credentials are what `CredsFactory` uses to build the per-cluster clients.
- **Persistence + Shared Kernel:** `store.Store`; `pb.Cluster` is kernel.

## Key decisions

- **Desired vs observed split** (I1) — the schema's central decision; observed is
  a projection (CQRS read model) rebuilt by refresh.
- **Endpoints vs node** — `endpoints.talos` are control-plane IPs the Talos API
  routes through, distinct from the node an operation targets (`talos-client.md`
  §2).
- **Gates on the cluster, not the job** — `rolloutsEnabled`/`mode` live here so
  one deliberate act arms an entire cluster (`rollout-safety.md` §3).
- **Multi-cluster-ready** — every record is namespaced by cluster name; v1
  operates one cluster but the schema needs no change to add more.
