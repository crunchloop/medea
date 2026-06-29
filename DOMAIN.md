# DOMAIN.md — Medea's domain map

The strategic map for `medea`, the external control plane for operating Talos
clusters. It exists so that a human or an AI agent can reason about the codebase
at the level of **bounded contexts**, **aggregates**, and a shared
**ubiquitous language** — and so that new code lands in the right context,
respects the right consistency boundary, and reuses the right word.

This is the *strategic* layer. It complements, and does not replace:

- [`PRD.md`](PRD.md) — what we are building and why (scope, goals, non-goals).
- [`design/`](design/README.md) — the *decision* records (why each subsystem
  looks the way it does: store, API/auth, talos-client, rollout-controller,
  rollout-safety).
- [`design/aggregates/`](design/aggregates/README.md) — the *per-aggregate*
  design records (consistency boundary, lifecycle, invariants, command/event
  surface). This file is the index over them; that directory is the detail.

When this doc disagrees with the code, **the code is authoritative** — file an
update. References below point at packages/symbols, never line numbers (they
rot).

---

## 1. Our DDD posture — how DDD maps onto *this* codebase

Medea is not a textbook DDD application and should not be refactored into one.
It makes deliberate architectural choices (PRD §8, the `design/` records) that
*reframe* the standard DDD building blocks. Read this table before reaching for
any DDD pattern — the mapping is the point.

| DDD concept | Textbook form | **How Medea does it (and why)** |
| --- | --- | --- |
| **Domain model** | Rich entities with behavior | **Anemic by design.** Aggregates are protobuf messages in [`gen/medea/v1`](gen/medea/v1) — pure data + getters, no methods. One `.proto` is the single source of type truth for the API, the store (proto wire bytes are the stored value), and the CLI (`design/datastore.md` §4). Behavior lives in *services*, never on the data. **Do not add methods to the proto types or wrap them in hand-written "rich" structs.** |
| **Aggregate** | Cluster of objects with one root | A **single record** keyed in the store (`Cluster`, `NodePool`, `Machine`, `Host`, `Rollout`, `MachineRollout`, `ClusterRollout`). The consistency boundary is one record / one write. There is no multi-record transaction; cross-record consistency is eventual, driven by reconcile. |
| **Repository** | Interface returning aggregates | [`store.Store`](internal/store) — a typed, per-resource surface (`GetCluster`, `PutNodePoolDesired`, `ListRolloutJobs`, …) over bbolt. Map "repository" → `store.Store`. The bbolt mechanics (buckets, revisions, watch) are platform; the interface is the domain seam. |
| **Application service** | Orchestrates a use case | The gRPC handlers in [`server`](internal/server) — intent verbs (`SetClusterVersions`, `CreateRollout`, `EnableRollouts`), server-side read-modify-write, invariant enforcement at the boundary. |
| **Domain service** | Stateless domain logic | The **reconcilers**: [`rollout.Reconciler`](internal/rollout) (per-node OS-upgrade state machine) and [`rollout.Executor`](internal/rollout) (job driver). The reconcile loop is the universal skeleton (PRD §8.2). |
| **Domain event** | State-carrying event | **Thin notification events.** The store's watch broadcaster emits `store.Event{Kind, Key, Revision}` after every committed write (`design/datastore.md` §5). Events *name what changed*, not the new value — clients re-fetch. Six kinds exist (§6). |
| **Anti-corruption layer** | Translate an external model | [`talos`](internal/talos) (over Talos `machinery`) and [`kube`](internal/kube) (over `client-go`). Reconcilers depend on small Medea-owned interfaces (`TalosOps`, `KubeOps`), never on upstream types (`design/talos-client.md` §1). |
| **CQRS** | Separate read/write models | **Desired vs observed.** *Desired* state (precious, persisted, written via CAS) is the write model; *observed* state (rebuildable, in-memory only, projected by [`refresh`](internal/refresh)) is the read model (`design/datastore.md` §2). |
| **Optimistic concurrency** | Version field + CAS | Every record carries a `revision`; desired writes are compare-and-swap (`store.ErrConflict` on mismatch); rollout-progress writes are last-writer-wins (single owner). Writer separation (API ↔ reconciler) keeps contention near zero. |
| **Transaction / unit of work** | DB transaction | One bbolt RW txn per write (one writer at a time). **No cross-aggregate transaction exists or should be assumed.** |

**Consequences for anyone (human or AI) changing code here:**

- Don't put behavior on the proto types. Put it in a handler (application
  service) or a reconciler (domain service).
- Don't reach for a second source of truth (a file, a cache trusted as truth).
  The store is canonical; observed is always re-derivable.
- A new external dependency goes behind a small interface in an ACL package,
  never imported directly by a reconciler.
- "Who needs to know when this changes?" → a `store.Event`, consumed by a
  watcher; not a direct call into another context.

## 2. Bounded context map

Boundaries were drawn from the **actual import graph**, not intuition. The four
top-level slices (`server`, `seed`, `refresh`, `rollout`) have *zero* edges
between each other; `store`+`gen` are the shared center; `talos`/`kube`/`creds`
are shared infrastructure.

```
            ┌──────────────────────── API host: internal/server ─────────────────────────┐
            │  (gRPC handlers — split across the two domain contexts by the language       │
            │   they speak: inventory verbs vs rollout/safety verbs)                       │
            └───────────────┬──────────────────────────────────────┬──────────────────────┘
                            │                                       │
              ┌─────────────▼─────────────┐         ┌───────────────▼────────────────┐
              │   CLUSTER INVENTORY        │         │   VERSION ROLLOUT  (CORE)       │
              │   (supporting)             │         │                                 │
              │   Cluster · NodePool ·     │ desired │   Rollout(job) · MachineRollout │
              │   Machine                  │◄────────┤   · ClusterRollout              │
              │   seed (bootstrap)         │  reads  │   rollout.Executor + Reconciler │
              │   refresh (observed proj.) │         │   + safety guard chain          │
              └─────────────┬─────────────┘         └───────────────┬─────────────────┘
                            │  repository + events                  │
              ┌─────────────▼───────────────────────────────────────▼─────────────────┐
              │   PERSISTENCE + SHARED KERNEL                                           │
              │   store.Store (repository, CAS, watch broadcaster) · gen/medea/v1 types │
              └─────────────┬───────────────────────────────────────┬─────────────────┘
                            │                                        │
        ┌───────────────────▼─────┐  ┌────────────────────┐  ┌──────▼──────────────────┐
        │ TALOS INTEGRATION       │  │ KUBERNETES INTEGR.  │  │ SECURITY & CREDENTIALS  │
        │ talos (ACL + image      │  │ kube (ACL over      │  │ creds · tlsgen · auth   │
        │ derivation) [generic]   │  │ client-go)[generic] │  │ [generic]               │
        └─────────────────────────┘  └────────────────────┘  └─────────────────────────┘
```

| Context | Type | Modules (packages) | Aggregates / responsibility |
| --- | --- | --- | --- |
| **Version Rollout** | **Core domain** | [`rollout`](internal/rollout); the rollout/safety handlers in [`server`](internal/server) (`CreateRollout`, `EnableRollouts`, `DisableRollouts`, `PauseRollout`, `ResumeRollout`, `ListRollouts`, `GetRollout`) | `Rollout` (job), `MachineRollout` (per-node execution), `ClusterRollout` (K8s-upgrade phase). The reason Medea exists: safe, observable version rollouts with the safety model baked in. |
| **Cluster Inventory** | Supporting | [`seed`](internal/seed), [`refresh`](internal/refresh); the inventory handlers in [`server`](internal/server) (`GetCluster`, `ListClusters`, `ListNodePools`, `ListMachines`, `SetClusterVersions`, `SetNodePoolVersion`, `RegisterHost`, `ListHosts`, `DeregisterHost`) | `Cluster`, `NodePool`, `Machine`, `Host`. The registry of what clusters/pools/nodes/bare-metal-hosts exist, their desired versions, and their observed reality. Seeding bootstraps it; refresh projects observed onto it. (`Host` is the v2 provisioning-inventory aggregate; v2-M1 ships register/list, the lifecycle reconciler lands in v2-M3.) |
| **Persistence + Shared Kernel** | Generic / kernel | [`store`](internal/store) (impl); [`gen/medea/v1`](gen/medea/v1) (kernel types) | The repository, optimistic concurrency, desired-state export, and the domain-event broadcaster. The proto types are the shared kernel every context speaks. |
| **Talos Integration** | Generic subdomain | [`talos`](internal/talos) | ACL over Talos `machinery` (`Version`, `UpgradeOS`, `EtcdSnapshot`) + installer-image/schematic derivation. *Note:* `DeriveInstallerImage` (preserve-schematic, bump-version) is genuine domain logic that lives here for cohesion (`design/talos-client.md` §3). |
| **Kubernetes Integration** | Generic subdomain | [`kube`](internal/kube) | ACL over `client-go` (`ListNodes`, `Drain`, `Cordon`/`Uncordon`, `NodeReady`, `KubeletVersion`). Outward client only — never in-cluster (PRD §8). |
| **Security & Credentials** | Generic subdomain | [`creds`](internal/creds), [`tlsgen`](internal/tlsgen), [`auth`](internal/auth) | Cluster credential storage (file-backed, never in bbolt), self-signed server TLS, bearer-token gRPC interceptors. |

The composition root is [`cmd/medea`](cmd/medea) (the CLI + `serve`); it wires
every context together and is the only place that imports them all.

## 3. Context relationships

- **Version Rollout → Cluster Inventory** (customer/supplier): rollout reads
  `Cluster` (endpoints, `rolloutsEnabled`, `mode`), `NodePool` (members,
  strategy, `paused`, desired version), and `Machine` (role) — all owned by
  Inventory. A confirmed `CreateRollout` *also* sets the pool's desired version
  (Inventory write) as part of authorizing the job.
- **Both domain contexts → Persistence** (conformist): both speak the shared
  kernel proto types and use `store.Store` as their repository.
- **Both domain contexts → Talos/Kube/Security** (ACL): only through the small
  `TalosOps`/`KubeOps` interfaces and the `creds.Store`; upstream types never
  leak past these packages.
- **Writer separation is a hard rule**: Inventory's API handlers own the
  `desired/` records (CAS); Rollout's reconcilers own the `rollouts/` records
  (LWW); refresh owns the in-memory observed projection. They touch disjoint
  key spaces (`design/datastore.md` §6).

## 4. Ubiquitous language (glossary)

Terms every context shares. Use these words in code, comments, commits, and
docs.

| Term | Meaning |
| --- | --- |
| **Cluster** | A managed Talos Kubernetes cluster. Aggregate root holding desired versions, endpoints, `mode`, and the `rolloutsEnabled` guard. |
| **NodePool** | A group of like nodes (`controlplane` or `workers`). Holds membership, the rollout `strategy`, the pool-level desired Talos version, and `paused`. The managed-node-group abstraction. v2 adds `replicas` + `selector` (reconciler-managed membership); `replicas == 0` + an explicit `members` list = the v1 behavior. |
| **Host** | A piece of bare metal Medea knows about *before* it is a cluster member — the v2 provisioning inventory. Identity = NIC MAC. Holds the owning pool, `labels` (matched by `NodePool.selector`), and a lifecycle `state`. v2-M1 only registers them (`Registered`); the provisioning reconciler drives the rest. |
| **Machine** | One node. Identity is its **Talos endpoint** (an address). Holds role + observed phase/versions/health. Mostly reconciler/refresh-owned. |
| **Desired state** | The precious, operator-set intent (target versions, membership, strategy). Persisted; only exists in Medea; written via CAS. |
| **Observed state** | The current reality (versions, health, readiness) re-read from the live cluster. A rebuildable in-memory cache, never persisted, never trusted as truth. |
| **Rollout (job)** | The explicit, audited `Rollout` record that *authorizes and tracks* an upgrade. In manual mode, nothing upgrades without one. See `design/aggregates/rollout.md`. |
| **MachineRollout** | The per-node execution **progress/state machine** of an OS upgrade (`Idle→Draining→Upgrading→WaitingHealthy→Done/Failed`). Persisted so a rollout resumes after a restart/reboot. |
| **ClusterRollout** | The cluster-wide K8s-upgrade **phase** (`Idle→Upgrading→Idle/Failed`). Driven by `ReconcileK8s` via the imported Talos `upgrade-k8s`; snapshot-before-K8s is mandatory. |
| **Rollout strategy** | `maxUnavailable`, `drainTimeout`, `haltOnFailure`, `snapshotBeforeControlPlane` — the per-pool safety knobs. |
| **maxUnavailable** | OS path: how many pool members may be not-Ready at once. v1 processes one node at a time, satisfying any value ≥ 1. |
| **Drain** | Cordon + evict pods, PDB-respecting, **no force**. A drain timeout *halts* the rollout (surfaces the blocking pod) rather than evicting. |
| **Halt-on-failure** | The core safety property: the first node that fails to drain/upgrade/become healthy stops the *entire* rollout. The difference between Medea and a `for` loop of `talosctl upgrade`. |
| **Snapshot-before-control-plane** | Mandatory etcd snapshot before mutating a control-plane node — the only undo on a single-member etcd. Snapshot failure aborts the rollout. |
| **rolloutsEnabled** | Per-cluster hard guard, default **off**, never set by seed. A cluster is actionable only after a deliberate `enable-rollouts`. Enforced at both job creation and execution. |
| **mode** | Per-cluster `ClusterMode`: `manual` (v1 default — rollouts only via explicit jobs) or `auto` (drift-reconcile; architected, rejected in v1). |
| **paused** | Per-pool flag halting rollout progression at the next safe point (between nodes, never mid-node). |
| **Plan / confirm** | `medea upgrade` defaults to a dry-run **plan**; `--confirm` is what creates the job and sets desired. |
| **Installer image / schematic** | The Talos installer image encodes the node's Image-Factory schematic (system extensions). Upgrades must **preserve the schematic** and bump only the version (`talos.DeriveInstallerImage`). |
| **Endpoint vs node** | *Endpoint* = control-plane IP the Talos API routes through; *node* = the machine an operation targets. Distinct in the Talos client. |
| **Revision** | Per-record monotonic write counter; the basis for both watch cursors and CAS. |
| **Watch event** | A thin `{kind, key, revision}` notification published after a committed write; clients re-fetch the object. |
| **Park-and-retry** | The reconciler's response to an unreachable cluster mid-reboot — back off and retry, *not* fail (`design/talos-client.md` §1, `errors.Is(err, ErrUnreachable)`). |
| **Seed** | The one-time bootstrap that reads a live cluster and writes Inventory *desired* = current reality (run with the server stopped). |
| **Refresh** | The continuous pass that reads the live cluster and writes the in-memory *observed* projection. |

## 5. Jargon decoder (opaque / easily-confused terms)

Read this before assuming what a name means — several words are overloaded.

- **"Rollout" means four different things.** Disambiguate every time:
  1. `pb.Rollout` — the **job** (authorization + audit + overall job state).
  2. `pb.MachineRollout` — **per-node execution progress** (the OS state machine).
  3. `pb.ClusterRollout` — **cluster K8s-upgrade phase**.
  4. "a rollout" in prose — the *act* of upgrading a pool/cluster.
- **Four different "state-ish" enums — do not conflate:**
  - `ClusterMode` (`manual`/`auto`) — *how* rollouts trigger.
  - `RolloutJobState` (`Pending/Running/Paused/Failed/Done/Cancelled`) — the **job's** lifecycle.
  - `RolloutState` (`Idle/Draining/Upgrading/WaitingHealthy/Done/Failed`) — a **MachineRollout's** per-node lifecycle.
  - `ClusterRolloutPhase` (`Idle/Upgrading/Failed`) — the **K8s** path phase.
- **Three independent "is this allowed / running" layers** (defense in depth):
  `rolloutsEnabled` (cluster hard guard) → `mode` (manual vs auto) → `paused`
  (pool pause) → job `state`. A rollout proceeds only when all align.
- **`Executor` vs `Reconciler`** (both in `internal/rollout`):
  - `Executor` — finds actionable **jobs** on enabled clusters, re-checks the
    guard, builds clients, and drives one job. The "should this run?" layer.
  - `Reconciler` — the per-pool/per-node **state machine** (drain → snapshot →
    upgrade → wait → uncordon, halt-on-failure). The "how to upgrade" layer.
- **`Store` is a repository, not a shop.** Also note there are *two* unrelated
  `Store` interfaces: `store.Store` (the bbolt resource repository) and
  `creds.Store` (the credential store). They share a name, nothing else.
- **`Machine.talos_endpoint` is an identity, not a URL.** It is the node's
  address, used as its key within a cluster. `NodePool.members` are the same
  addresses.
- **`seed` vs `refresh`** both read the live cluster but write different things:
  seed writes **desired** (once, server stopped); refresh writes **observed**
  (continuously, server running). Confusing them is a source-of-truth bug.
- **"observed" is never persisted.** If you find yourself writing observed to
  bbolt, stop — that violates the desired/observed split (`design/datastore.md` §2).
- **`Cluster.desired.talosVersion` vs `NodePool.desired.talosVersion`:** the
  pool value `""` means *inherit the cluster's* (`rollout.targetVersion`).

## 6. Domain events

All events are thin `store.Event{Kind, Key, Revision}` published by the store's
in-process broadcaster after a committed write (`design/datastore.md` §5;
surfaced over the wire as `WatchEvent`). They notify; consumers re-fetch.

| Kind (`store.EventKind`) | Emitted when | Owner (writer) |
| --- | --- | --- |
| `cluster` | A `Cluster` desired record is written (incl. `enable/disable-rollouts`) | Cluster Inventory (API) |
| `nodepool` | A `NodePool` desired record is written (versions, `paused`) | Cluster Inventory (API) |
| `machine` | A `Machine` identity record is written (seeding) | Cluster Inventory (seed) |
| `host` | A `Host` inventory record is written/removed (register/deregister; later the provisioning reconciler) | Cluster Inventory (API; later provisioning reconciler) |
| `machine_rollout` | A node's `MachineRollout` progress transitions | Version Rollout (reconciler) |
| `cluster_rollout` | A `ClusterRollout` phase transitions (K8s path) | Version Rollout (reconciler) |
| `rollout_job` | A `Rollout` job is created or changes state | Version Rollout (API + executor) |

Consumers: the gRPC `Watch` stream (CLI `rollout status -w`, future UI) and
reconnecting clients (resume via `since_revision`). Note: observed-state changes
do **not** emit events (observed is an in-memory projection, never a store write).

## 7. Aggregate & invariant catalog

The per-aggregate design records — consistency boundary, lifecycle/state
machine, invariants (each tied to the command that enforces it and *why*),
command surface, event surface, cross-context deps, and key decisions — live in
[`design/aggregates/`](design/aggregates/README.md). That README is the catalog
index. Start with the core: [`design/aggregates/rollout.md`](design/aggregates/rollout.md).
