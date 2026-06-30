# PRD — `medea`: an external control plane for operating Talos clusters

**Status:** Draft (Pass-1); v1 implemented (M0–M4); v2 scoping (provisioning)
**Author:** @bilby91
**Date:** 2026-06-25 (initial draft); 2026-06-25 (Pass-1 — store, protocol, Talos-client, and state-model decisions resolved); 2026-06-26 (rollout-safety — explicit-job trigger, per-cluster mode + rolloutsEnabled); 2026-06-28 (v2 scoping — provisioning plane promoted from deferred; pillar sequencing v2→v3→v4)
**Repo:** built in-place under `medea/` in `github.com/bilby91/talos-cluster`, which converges into Medea over time (§13 #11). Go module path: `github.com/crunchloop/medea` from day one.
**Visibility:** private
**License:** Apache-2.0 (to be committed day 1 of the extracted repo)
**Language version floor:** Go 1.25 (matches the Talos / COSI / controller ecosystem we call into)
**Primary consumer:** the operator of the `bilby91/talos-cluster` bare-metal cluster (3× Beelink, netboot/PXE provisioned)
**Driving issue:** n/a (greenfield)
**Companion repo(s):** `bilby91/talos-cluster` (the cluster config — machine configs, netboot, manifests this control plane operates against)

> **Name.** "Medea" — in the Argonautica, Medea is the one who brought down Talos: she bewitched the bronze giant and pulled the pin that held his ichor, draining his life. An apt namesake for a control plane that holds power over Talos nodes — drain, upgrade, reset. The binary is `medea`. (Resolved, §13 #12.)

---

## 1. Summary

Medea is an **external, self-hosted control plane** that operates one or more Talos Linux Kubernetes clusters — providing the lifecycle automation that makes a cluster feel *managed* (à la EKS/AKS): safe version rollouts, node provisioning, auto-repair, and backup/DR. It is the "lifecycle brain" layered on top of systems that are already APIs: the **Talos machine API** (gRPC), a bare-metal **provisioning plane** (netboot/PXE/matchbox), and the target cluster's **kube-apiserver** (used only as a client, for drain/cordon).

The headline architectural commitment — and the one that distinguishes Medea from the Cluster API (CAPI) ecosystem — is that **Medea does not run inside the cluster it operates, and its API is not Kubernetes**. A control plane whose job is to operate a cluster must not depend on that cluster to function. Medea runs as a standalone service with its own datastore (the single source of truth for desired and observed state), its own API, and its own CLI. This mirrors how real managed-Kubernetes control planes work — they live in the provider's infrastructure, not in your cluster — and how Sidero's own **Omni** is built (a standalone service on the COSI runtime, *not* CRDs in the workload cluster).

v1 ships a single, high-leverage capability end-to-end: **safe, observable version rollouts** (Talos OS + Kubernetes) across an existing cluster. It exercises the core reconcile loop every later feature reuses, and delivers the operation an operator performs most often.

## 2. Motivation

The `talos-cluster` cluster is operated by hand today: `talosctl upgrade` / `upgrade-k8s` run manually, node by node, with the operator eyeballing drain and health. That is error-prone (no enforced drain, no halt-on-failure, no record of in-flight state) and it does not scale past a couple of nodes or a single operator. The pain that justifies the work:

- **Upgrades are the most frequent and most dangerous day-2 operation**, and today they have no safety rail. A bad image rolled to all three nodes in sequence takes out the cluster.
- The cluster runs a **single control-plane node + single-member etcd** (no HA), so a control-plane upgrade has exactly one undo: an etcd snapshot taken first. Nothing enforces that today.
- We want managed-K8s ergonomics (`one command → safe fleet upgrade`) **while owning the stack**, for learning and control.

Ecosystem gap analysis:

- **Cluster API (CAPI) + the Talos providers (CABPT/CACPPT/Sidero Metal)** does declarative cluster lifecycle, but the API surface *is* Kubernetes CRDs running in a management cluster. That reintroduces a K8s dependency for the thing operating K8s, and is heavyweight for a 3-node homelab.
- **Sidero Omni** is exactly the right shape (external, COSI-based, not-in-cluster) but is a commercial SaaS/product — adopting it is the opposite of "own the stack."
- **`system-upgrade-controller` (SUC)** does rolling upgrades but runs *as a workload inside the cluster* it upgrades — the in-cluster coupling we are explicitly rejecting (§4).
- **`talosctl` alone** is the right primitive but has no desired-state store, no reconcile loop, no halt-on-failure, no resume-after-reboot.

Honest read: there is a real gap at *"external, owned, Talos-native lifecycle control plane, homelab-scale."* Medea fills it.

## 3. Goals

- A **standalone service** that operates Talos clusters and **does not run inside, or depend on, the cluster it manages**.
- **Datastore is the single source of truth** for desired + observed state; the API mutates the store; reconcilers drive reality toward it.
- **Safe version rollouts (v1):** edit a target version → Talos OS and/or Kubernetes upgrade rolls node-by-node, respecting drain, `maxUnavailable`, and **halting on the first node that fails to converge**.
- **Resume-safe across the managed cluster's own outages** — including a control-plane node reboot that takes the kube-apiserver away mid-rollout. State lives in Medea's store, so reconcile resumes from where it left off.
- **Snapshot-before-control-plane** — never mutate a control-plane node without first taking an etcd snapshot (the only undo on a non-HA cluster).
- **API-first**, with a thin CLI (`medea`) over it; observable rollout progress.
- Architected so that **provisioning, auto-repair, and backup** (the other three pillars) slot in behind the same reconcile-loop + datastore skeleton without rework.

## 4. Non-goals

- **Running as Kubernetes CRDs / an in-cluster operator.** Explicitly rejected — it reintroduces the circular dependency (can't use a cluster's API to create or repair that cluster) and couples the fixer's lifecycle to the patient's. This is the core architectural stance, not a v1 cut.
- **Bare-metal provisioning / hardware inventory (v1).** Architected for (the "provisioning plane", §8) but **deferred** — the v1 rollout feature operates on *existing* nodes and needs none of it. Building it first would mean designing the hardware-inventory model before we need it.
- **Auto-repair (v1).** Deferred; depends on provisioning being able to reprovision a dead node.
- **HA control plane / multi-master orchestration (v1).** The target cluster is single-master; Medea must handle that safely (snapshot-first) but does not yet *create* HA control planes.
- **Multi-cluster fleet management UI (v1).** The data model is multi-cluster-capable, but v1 targets one cluster and ships no web UI.
- **A declarative `apply -f <file>` workflow as the source of truth.** The store is the truth (§13 decision). A declarative input *format* that POSTs to the API may come later, but there is no canonical on-disk cluster file.
- **Replacing GitOps for app/add-on management.** Cilium/MetalLB/CNPG/etc. stay in Argo CD/Flux. Medea operates the *cluster*, not the workloads on it.

## 5. Users & use cases

1. **The cluster operator (primary).** Runs `medea upgrade --k8s v1.36.2` or `medea upgrade --talos v1.13.6 --pool workers`, watches `medea rollout status`, pauses a bad rollout. Today this person is running `talosctl` by hand.
2. **Automation / CI (later).** A scheduled job or pipeline calls the Medea API to roll a tested version across the fleet. v1 exposes the API that makes this possible; no shipped automation yet.
3. **Future: a fleet operator** managing multiple Talos clusters from one Medea instance. The data model supports it; v1 does not exercise it.

## 6. Scope (v1)

In scope:

- **Medea service**: a single Go binary running reconcile loops, an embedded datastore, and an API endpoint.
- **Datastore** holding `Cluster`, `NodePool`, and `Machine`/node-state resources (desired + observed), plus rollout progress. Source of truth.
- **Version rollout reconciler**, handling the two distinct Talos upgrade mechanisms:
  - **Talos OS** (`talosctl upgrade`): per-node, atomic A/B + reboot, orchestrated node-by-node by Medea (cordon → drain → upgrade → wait-healthy → uncordon).
  - **Kubernetes** (`talosctl upgrade-k8s`): cluster-orchestrated by Talos itself; Medea triggers and monitors to completion.
- **Safety rails**: `maxUnavailable`, drain (PDB-respecting) with timeout, **halt-on-failure**, **snapshot-before-control-plane**, resume-after-reboot.
- **CLI** (`medea`) over the API: read cluster/pool/node state, set target versions, watch + pause rollouts.
- **Talos API client** (config-read, upgrade, upgrade-k8s, etcd snapshot, health) and **kube-apiserver client** (cordon/drain/node status), both as outward clients.
- **State seeding**: register the existing cluster + its nodes into the store from current talosconfig/kubeconfig (no provisioning).

Deferred past v1 — now **scoped as v2–v4** in dependency order (2026-06-28; see
§13 and §14):

- **Provisioning plane (v2)** — Layer-0: a `Host` inventory aggregate +
  `NodePool` replicas/selector, a Matchbox driver (absorbing `netboot/`),
  spec-based machine-config generation, Image-Factory schematic resolution, and
  a join-existing-cluster reconciler. v2 *adds nodes to an existing cluster*
  (scale-out / replace); new-cluster creation stays deferred. Power-agnostic (the
  `Power` interface is a v4 seam). Design: `design/provisioning-plane.md`.
- **Backup scheduler + restore (v3)** — schedule/retention/destination for etcd
  snapshots, and the restore flow (which control-plane auto-repair needs). The
  v1 ad-hoc pre-mutation snapshot is its seed. Design: `design/backup.md` (planned).
- **Auto-repair reconciler (v4)** — failure detection + the `Power` driver
  (WoL/smart-plug/Redfish); reprovision a dead node. Builds on v2 + v3. Design:
  `design/auto-repair.md` (planned).

Still deferred (no version yet):

- **Declarative `apply -f` input format**; web UI; multi-cluster fleet UI.
- **mTLS/OIDC auth** hardening (v1 runs with a bearer token over TLS — see §13).

## 7. Public API sketch

The surfaces a consumer touches: the **CLI**, the **API resources** (datastore-backed), and the **wire API**.

### 7.1 CLI

```bash
# Read state (the single pane of glass — from the store, not the cluster)
medea get clusters
medea get nodepools --cluster home
medea get nodes --pool workers

# Set a target version → triggers a rollout (mutates desired state in the store)
medea upgrade --cluster home --k8s v1.36.2          # K8s, whole cluster
medea upgrade --cluster home --talos v1.13.6 --pool workers   # Talos OS, one pool

# Observe & control an in-flight rollout
medea rollout status --pool workers                 # phase, per-version counts, current node
medea rollout pause  --pool workers
medea rollout resume --pool workers
```

Design notes:

- **Imperative verbs that mutate the store**, not `apply -f`. The store is the source of truth (§13); the CLI is a typed client over the API. A bad re-run of `apply` can't fight the reconciler because there is no second source of truth to drift.
- **`upgrade` sets a field; it does not drive the rollout.** The reconciler observes the new desired version and rolls. The CLI returns immediately; `rollout status` is how you watch.
- **`--pool` scopes Talos upgrades; `--k8s` is cluster-wide** because `upgrade-k8s` is inherently cluster-orchestrated (see §8.3).

### 7.2 API resources (datastore-backed; *not* CRDs)

These are records in Medea's store, surfaced over the wire API. Shown as YAML for readability only — there is no canonical YAML file.

```yaml
Cluster:
  name: home
  desired:   { talosVersion: v1.13.5, kubernetesVersion: v1.36.1 }
  observed:  { kubernetesVersion: v1.36.1, controlPlaneReady: true }
  endpoints: { talos: [10.0.0.10], kube: 10.0.0.10:6443 }
  # credential refs resolved from Medea's own secret store, not the cluster

NodePool:
  cluster: home
  name: workers
  role: worker                         # controlplane | worker
  members: [10.0.0.11, 10.0.0.12]   # node identities; v1 = existing nodes
  desired:  { talosVersion: "" }       # "" = inherit Cluster
  strategy:
    maxUnavailable: 1
    drainTimeout: 5m
    haltOnFailure: true
    snapshotBeforeControlPlane: true   # only meaningful for role: controlplane
  paused: false

Machine:                               # one node; mostly reconciler-owned
  cluster: home
  pool: workers
  talosEndpoint: 10.0.0.12
  observed: { phase: Ready, talosVersion: v1.13.5, kubernetesVersion: v1.36.1, healthy: true }
  rollout:  { state: Idle }            # Idle|Draining|Upgrading|WaitingHealthy|Done|Failed
```

Design notes:

- **`Machine.rollout.state` lives in the store, not on the node.** That is what makes rollouts resume-safe across a control-plane reboot: when the managed apiserver vanishes, Medea's record of "node X was mid-upgrade" survives in Medea's own store.
- **No `apiVersion`/K8s envelope.** These are Medea's domain objects.
- **`members` are addresses in v1** (existing nodes). When provisioning lands, a pool gains `replicas` + a hardware selector and `members` becomes reconciler-managed.

### 7.3 Architecture sketch

```
                 ┌──────────────── operator / CI ────────────────┐
                 │                medea CLI                       │
                 └───────────────────┬────────────────────────────┘
                                     │  wire API (gRPC, §13)
                 ┌───────────────────▼────────────────────────────┐
                 │  MEDEA  (standalone service — runs on a VM /     │
                 │          container / systemd unit, NOT in the    │
                 │          managed cluster)                        │
                 │  ┌──────────────┐   ┌───────────────────────┐    │
                 │  │ API server   │   │ Datastore (embedded)  │◄───┼─ source of truth
                 │  └──────┬───────┘   │  desired + observed   │    │  (desired+observed
                 │         │           │  + rollout progress   │    │   + rollout state)
                 │  ┌──────▼───────────┴───────────────────────┐    │
                 │  │ Reconcilers:  rollout │ (later) provision │    │
                 │  │               repair  │ (later) backup    │    │
                 │  └──────┬─────────────┬──────────────┬───────┘    │
                 └─────────┼─────────────┼──────────────┼────────────┘
                  Talos gRPC│   kube-apiserver│   PXE/matchbox│ (Layer 0, deferred)
                  upgrade,  │   cordon/drain  │   boot bare   │
                  snapshot, │   (client only) │   metal       │
                  health    ▼                 ▼               ▼
              ┌─────────────────────────── target cluster ───────────────────────────┐
              │  talos-t0g-yh8 (cp) · talos-4rr-xdo (worker) · talos-u2x-ev5 (worker) │
              └───────────────────────────────────────────────────────────────────────┘
```

## 8. Architecture

### 8.1 Layering

```
medea/                             (Go module `github.com/crunchloop/medea`; repo converges here — §13 #11)
├── cmd/medea/                      CLI + service entrypoints
├── internal/
│   ├── api/                        wire API (gRPC server + handlers)
│   ├── store/                      datastore: schema, persistence, watch
│   ├── model/                      Cluster / NodePool / Machine domain types
│   ├── reconcile/
│   │   ├── rollout/                v1 — version rollout reconciler + state machine
│   │   ├── provision/              (deferred) Layer-0 driver
│   │   ├── repair/                 (deferred)
│   │   └── backup/                 (deferred)
│   ├── talos/                      Talos gRPC client wrapper (upgrade, snapshot, health)
│   └── kube/                       kube-apiserver client wrapper (cordon/drain)
└── PRD.md, design/
```

### 8.2 Key architectural choices

- **External, not in-cluster (and not CRDs).** Ties to the core goal and non-goal. A cluster operator must outlive cluster outages and avoid the bootstrap cycle. Rejected: CAPI (management cluster is still K8s), SUC (in-cluster workload).
- **Datastore is the single source of truth.** Rejected: file/git as truth (drift between file and store; reconcile-against-file complexity). The store is canonical; CLI mutates it; reconcilers converge reality. This is the AWS-API model.
- **Only *desired* state is precious; *observed* is a cache.** Observed state (current versions, health) is always re-readable from Talos/kube, so it is a refreshable projection, never authoritative. The only data that exists *solely* in Medea is desired config (target versions, pool membership, strategy) plus, transiently, in-flight rollout progress. This bounds Medea's durability/backup burden to a tiny amount of desired-state config and shapes the store schema (desired vs observed are separate, observed is rebuildable on boot).
- **Reconcile loop as the universal skeleton.** Every pillar (rollout now; provision/repair/backup later) is a controller over `desired vs observed` in the store. v1 proves the skeleton on the highest-frequency operation.
- **Reuse, don't rebuild, the layers below.** Talos gRPC, the kube-apiserver, and (later) matchbox/PXE are called outward as clients. Medea owns lifecycle orchestration only.
- **Talos as the reference idiom.** The external-service + own-resource-store shape follows Talos/Omni/COSI precedent rather than the K8s-operator pattern (Appendix C).

### 8.3 The two upgrade mechanisms (why rollout has two code paths)

Talos exposes upgrades differently for OS vs Kubernetes, and the reconciler must respect that:

- **Talos OS** — `talosctl upgrade` is **per-node**: an atomic A/B image swap and reboot. Medea drives the loop: pick next node within `maxUnavailable`, cordon + drain, call upgrade, wait for the node to return `Ready` + Talos-healthy + `observedVersion == desired`, uncordon, repeat. Halt the whole rollout on the first node that fails to converge.
- **Kubernetes** — `talosctl upgrade-k8s` is **cluster-orchestrated by Talos itself** (it walks control-plane components and kubelets). Medea *triggers* it once and *monitors* to completion rather than driving node-by-node. `maxUnavailable` does not apply the same way.

For control-plane nodes (single-master, non-HA): **take an etcd snapshot first** (`snapshotBeforeControlPlane`), and treat the apiserver disappearing during the reboot as expected — park and resume from store state.

### 8.4 Talos client — import, never shell out

Medea talks to Talos exclusively through **imported Go packages**, never by
shelling out to a `talosctl` binary. This keeps the interaction typed and
removes any runtime dependency on an external binary or output parsing. It
splits across two Talos modules:

- **`pkg/machinery` (lightweight, externally versioned):** OS `upgrade`, etcd
  `snapshot`, and health/version reads are clean RPCs here. M1/M2 stay on
  this module only.
- **Talos *main* module (heavier):** the `upgrade-k8s` orchestration
  (rewriting control-plane static-pod manifests, rolling kubelets) is **not**
  a single RPC — it lives in Talos's own upgrade package in the main module.
  Importing it is what "no shelling" costs: a large dependency tree and
  **version coupling** between Medea's imported upgrade logic and the
  cluster's Talos release (§12). The K8s path (M3) is quarantined behind an
  interface so this dependency touches one package, not the whole codebase.

Likewise, Medea reaches the target cluster's kube-apiserver through the
typed `client-go` libraries (cordon/drain/node status) — again as an outward
client, never in-cluster.

## 9. Test strategy

### 9.1 Unit (no external dependencies)

- `reconcile/rollout`: the per-node state machine — version diffing, `maxUnavailable` accounting, halt-on-failure, resume-from-mid-state. Driven by a faked Talos/kube client.
- `store`: persistence round-trips, watch semantics, crash/restart recovery (reopen store → state intact).
- `model`: inheritance (`NodePool.desired.talosVersion == "" → Cluster default`).

### 9.2 Integration (Talos + kube)

- Against a **Talos `docker`/QEMU cluster** spun up in CI (`talosctl cluster create`): real `upgrade-k8s`, real cordon/drain, real etcd snapshot. Assert version converges and halt-on-failure trips on an injected bad version.

### 9.3 E2E (full stack)

- Against the actual 3-node Beelink cluster (manual, pre-release): a real K8s patch upgrade and a real Talos worker-pool upgrade, observed via `medea rollout status`.

### 9.4 CI

- GitHub Actions, Linux runners. `go vet`, `golangci-lint`, `-race` on unit tests. Talos-in-docker for the integration tier (gated, slower job).

## 10. Migration plan

n/a — greenfield standalone service; no existing system to migrate off. (Initial state is seeded from the live talosconfig/kubeconfig; that is a one-time import, not a migration.)

## 11. Companion changes

n/a — single project. The `talos-cluster` repo provides the cluster Medea operates against but **does not depend on Medea**; Medea is a consumer of its Talos endpoints, not the reverse.

## 12. Risks & mitigations

| Risk | Mitigation |
| --- | --- |
| Control-plane upgrade on a non-HA, single-member etcd bricks the cluster. | `snapshotBeforeControlPlane` is mandatory for `role: controlplane`; halt-on-failure; document restore path before first CP rollout. |
| Medea loses the apiserver mid-rollout (CP reboot) and double-upgrades or skips a node. | Rollout state persisted in Medea's store; reconciler is idempotent and resumes from `Machine.rollout.state`. Covered by a unit test. |
| Drain hangs forever on a stuck PDB. | `drainTimeout`; on timeout → halt rollout, surface the blocking pod, do not force. |
| `upgrade-k8s` semantics differ from OS upgrade and get conflated. | Two explicit code paths (§8.3); integration test exercises both. |
| Medea's store is lost (disk dies). | Only *desired* state is precious (§8.2); observed is rebuilt from the cluster on boot. Back up the small desired-state set (export). Medea being *down* never harms the cluster (it only stops *operating* it) — a deliberate property of being external. |
| "No shelling" couples Medea's build to a Talos release (imported `upgrade-k8s` lives in Talos's main module). | Quarantine the K8s-upgrade dependency behind one interface (§8.4); pin/track a supported Talos version range; the lightweight `machinery` module (OS/snapshot/health) is externally versioned and stable. |
| Scope creep into provisioning before rollout is solid. | Provisioning is an explicit non-goal for v1 (§4); data model leaves room but no code. |

## 13. Decisions

Resolved during initial draft (2026-06-25):

1. **External, not in-cluster.** Medea runs as a standalone service and is not a Kubernetes operator/CRD set. Rationale: a cluster operator must not depend on the cluster it operates (bootstrap cycle + blast radius). Core stance (§4).
2. **Datastore is the single source of truth.** No canonical on-disk cluster file; the CLI mutates store state via the API; reconcilers converge reality. Rejected file/git-as-truth to avoid drift.
3. **v1 = version rollouts only.** Highest-frequency, highest-risk day-2 op; proves the reconcile skeleton; needs no provisioning. Provisioning/repair/backup deferred (§6).
4. **Two upgrade code paths.** OS = per-node Medea-driven; K8s = Talos-orchestrated, Medea-monitored (§8.3).
5. **Snapshot before control-plane mutation.** Mandatory on non-HA; the only undo.
6. **Language = Go.** Matches Talos/COSI/kube client ecosystem and gRPC tooling.

Resolved during Pass-1 (2026-06-25):

7. **State store = embedded.** Embedded KV (bbolt) / SQLite — zero external deps, single binary, no dependency on the thing it manages. Rejected: Postgres (adds a dep), COSI (maximally Talos-native but steeper; revisit only if we later want COSI's resource semantics). Schema separates desired (precious) from observed (rebuildable) per #13.
8. **Wire API protocol = gRPC.** Typed contracts are a priority; gRPC gives a strongly-typed CLI client now and a clean path to a UI later. Rejected REST/JSON despite faster solo iteration — types won.
13. **Observed state is a cache, not truth.** Only desired config (+ transient rollout progress) is precious and must persist; observed is re-read from Talos/kube and rebuildable on boot. Shrinks the durability/backup surface (§8.2, §12).
14. **The daemon is justified specifically by resume-safety.** A long-running service + store is warranted *because* rollouts must survive a crash or a control-plane reboot mid-flight — not by default. If resume were ever dropped, the architecture collapses to a CLI + state file.
15. **No shelling out to `talosctl`; import Talos Go packages.** Typed, no external-binary runtime dep. Accepts the cost: importing `upgrade-k8s` from Talos's main module couples the build to a Talos release; quarantined behind one interface (§8.4, §12).
12. **Name = Medea.** In the Argonautica, Medea is the one who brought down Talos (drained his ichor) — apt for a control plane with power over Talos nodes. Binary `medea`. Considered and rejected: `forge` (overloaded/unsearchable), `ichor`, `anvil`, `warden`.
9. **Auth = bearer token over TLS (v1).** Server presents a self-signed cert (LAN service); a shared bearer token is checked by a gRPC interceptor. Rejected plaintext+token (cleartext credential on the LAN). mTLS / OIDC / RBAC deferred behind the interceptor seam. Credentials (talosconfig/kubeconfig) live in a separate file-backed `CredentialStore`, never in bbolt, never exported (`design/api-and-auth.md`).
11. **Repo: convergence, not extraction.** The end state is that `bilby91/talos-cluster` *becomes* Medea — its current contents are hand-run early versions of Medea's own subsystems (`netboot/` = the Layer-0 provisioning plane; `controlplane.yaml`/`worker.yaml`/`patches/`/`cilium/` = the declarative cluster definition + add-ons Medea will own; `_out/` = seed state; `scripts/` = future reconcilers). So we **do not** spin up a separate repo or `git init` the subtree; we build in-place and the whole repo restructures under Medea when it's real. Consequence: **Go module path is `github.com/crunchloop/medea` from day one** (not `…/talos-cluster/medea`) to avoid an import rewrite at convergence.

Resolved during rollout-safety review (2026-06-26):

16. **Rollout trigger = explicit jobs; per-cluster `mode` (manual default).** Reverses the drift-reconcile stance of `rollout-controller.md §1`: editing `desired` is inert in v1; rollouts run only from a confirmed `Rollout` job. `auto` (drift-reconcile) is architected but deferred and never for control-plane (`design/rollout-safety.md`).
17. **`rolloutsEnabled` per cluster, default off.** Never set by seed; enforced at job creation *and* execution. The hard guard that makes the live production cluster un-rollout-able by accident — it is simply never enabled.
18. **Control-plane: no extra confirmation gate**, but snapshot-before-control-plane stays mandatory. Plan-then-`--confirm` applies to all pools.

Resolved during v2 scoping (2026-06-28) — full detail in `design/provisioning-plane.md`:

19. **Pillar sequencing: Provisioning (v2) → Backup/Restore (v3) → Auto-repair (v4).** Dependency order; each independently shippable. Restore precedes repair because control-plane repair needs it.
20. **Provisioning drives Matchbox.** Medea owns Matchbox profiles/groups + rendered machine configs and orchestrates the existing dnsmasq/TFTP iPXE chainload (absorbs `netboot/`, decision #11). Rejected: Medea-as-boot-server; Sidero Metal (CRD/in-cluster, contra App. B).
21. **Inventory = a MAC-keyed `Host` aggregate; `NodePool` gains `replicas` + `selector`; membership becomes reconciler-managed.** The CAPI/Metal3/Sidero shape, minus the BMC assumption.
22. **v2 scope = add nodes to an existing cluster** (scale-out / replace), reusing the cluster's secrets. New-cluster / first-CP bootstrap deferred.
23. **Medea generates machine config from a spec** (`Cluster`/`NodePool` spec + per-node patches) and is its source of truth.
24. **Medea resolves schematics via the Image Factory API** (per-pool extension set → pinned schematic ID).
25. **Medea owns the cluster machine-secrets bundle** — captured from the live cluster into the `CredentialStore`, used to mint join configs; never in bbolt, never exported.
26. **Provisioning is power-agnostic.** Stage boot + wait (manual/WoL power-on); a `Power` interface is a v4 seam (Beelinks have no BMC).

Resolved during v3 scoping (2026-06-28) — full detail in `design/backup.md`:

27. **Backup destination = a pluggable `BackupTarget`** (local + S3/MinIO first; 1Password later). Off-box durability for etcd snapshots.
28. **A backup is a full DR bundle** — etcd snapshot + desired-state export + cluster secrets bundle. Because it contains secrets, it is **client-side encrypted (`age`)** with the decryption identity escrowed out-of-band (1Password). This *supersedes, for the encrypted DR path only*, the v1 "credentials are never serialized" stance (`api-and-auth.md` §5, `datastore.md` §9); the plaintext desired-state auto-export stays secret-free.
29. **Schedule = fixed interval + keep-N per cluster**, `backup.enabled` default off (same posture as `rolloutsEnabled`). Cron/GFS tiers deferred.
30. **Restore = guarded plan-then-confirm `RestoreEtcd` primitive** (stronger confirmation than upgrades — it re-bootstraps single-member etcd), exposed via CLI and callable by v4 auto-repair. This is why restore lands in v3, before repair.

Resolved during v4 scoping (2026-06-28) — full detail in `design/auto-repair.md`:

31. **Detection = sustained unreachable, debounced** (kube-NotReady AND Talos-unreachable past a threshold, across consecutive refresh passes), suppressed while the node has a rollout/provision in flight. Reuses the observed-state refresh loop.
32. **Repair action = reprovision the same host** (worker: drain→deprovision→reprovision via v2→rejoin; CP: reprovision→`RestoreEtcd`→rejoin).
33. **Control-plane repair is semi-automatic** — workers auto (gated); a CP failure is detected and a restore plan prepared, but a human `--confirm`s the etcd restore (too destructive to automate on single-master). Fully-auto CP repair becomes reasonable only once the cluster is HA.
34. **`Power` is a pluggable driver with graceful degradation** — smart-plug/Redfish (true cycle → hands-off) | WoL (cleanly-off only) | none (stage reprovision + notify a human to power-cycle). Concrete Beelink impl pinned after the hardware capability check.
35. **Auto-repair safety**: per-cluster `autoRepairEnabled` (default off, never seeded), one repair at a time per cluster, cooldown + max-attempts (no repair storms), and never-repair-an-intentionally-down-node. Inherits the `rollout-safety.md` posture.

To be resolved (non-blocking — do not block M1/M2 code):

10. **Where Medea runs / is deployed.** systemd unit on a small always-on box vs container vs operator workstation. Must be somewhere that is *not* the managed cluster. *Candidate: the netboot host on `10.0.0.0/24`.*

## 14. Milestones

- **M0 — Bootstrap.** PRD (this doc), `design/` index, status tracker. Resolve §13 Pass-1 decisions. *(in progress)*
- **M1 — Skeleton + state seeding.** Go module, embedded store, domain model, API + CLI read path; seed the live cluster's `Cluster`/`NodePool`/`Machine` state from talosconfig/kubeconfig. `medea get ...` works against the real cluster (read-only). Blocks on `design/datastore.md`. ~1–1.5 wk.
- **M2 — Rollout reconciler (OS path).** Per-node Talos OS upgrade with drain, `maxUnavailable`, halt-on-failure, resume. Worker-pool upgrade end-to-end on the real cluster. Blocks on `design/rollout-controller.md`. ~1.5–2 wk.
- **M3 — Rollout reconciler (K8s path) + CP safety.** `upgrade-k8s` trigger/monitor; `snapshotBeforeControlPlane`; control-plane rollout with resume-after-reboot. ~1 wk.
- **M4 — Hardening.** Integration tests (Talos-in-docker), auth, deployment artifact (systemd/container), docs. ~1 wk.

Total: ~5–6 calendar weeks for v1, depending on how much the K8s-path and CP-reboot resume edge cases bite.

**v1 status (2026-06-28):** M0–M4 implemented. Both upgrade paths and the safety
model are tested (unit `-race`, docker integration, QEMU worker + control-plane).

### v2+ (post-v1 pillars, 2026-06-28 scoping)

Sequenced by dependency (§13 #19). Detail in the per-pillar design records.

- **v2 — Provisioning plane** (`design/provisioning-plane.md`). Add nodes to an
  existing cluster: `Host` inventory + `NodePool` replicas/selector (v2-M1),
  Matchbox driver + spec-based config/schematic generation (v2-M2), the
  join-existing reconciler (v2-M3), scale-in/replacement + hardening (v2-M4).
- **v3 — Backup + restore** (`design/backup.md`). `BackupTarget` seam + bundle +
  `age` encryption (v3-M1), interval/keep-N scheduler + history (v3-M2), the
  guarded `RestoreEtcd` primitive control-plane repair depends on (v3-M3).
- **v4 — Auto-repair** (`design/auto-repair.md`). Detector + `RepairJob` +
  safety gates (v4-M1), worker auto-repair + the `Power` seam/WoL + degraded
  mode (v4-M2), semi-automatic control-plane recovery via `RestoreEtcd` (v4-M3).

---

## Appendix A — Reference material

- Talos machine API / upgrades: https://www.talos.dev/latest/talos-guides/upgrading-talos/
- `talosctl upgrade-k8s`: https://www.talos.dev/latest/kubernetes-guides/upgrading-kubernetes/
- Sidero Omni (prior art for external Talos control plane): https://www.siderolabs.com/platform/saas-for-kubernetes/
- COSI (Common Operating System Interface) resource runtime: https://github.com/cosi-project/runtime
- Cluster API: https://cluster-api.sigs.k8s.io/
- system-upgrade-controller: https://github.com/rancher/system-upgrade-controller

## Appendix B — Why not Kubernetes CRDs / CAPI

The natural default is to model the control plane as CRDs reconciled by an operator in a management cluster (the CAPI pattern). Rejected because:

1. **Bootstrap cycle.** A CRD needs a running apiserver; you cannot use a cluster's API to create that cluster's first control-plane node.
2. **Blast radius.** An in-cluster operator shares the fate of the cluster it is meant to repair — the fixer dies with the patient. We hit this concretely while designing rollouts: an in-cluster operator reboots its own control-plane node and loses the apiserver mid-upgrade. External placement dissolves the problem.
3. **The managed-K8s analogy argues against it.** EKS/AKS control planes run in the provider's infrastructure, external to your cluster — which is the experience we are reproducing.

The convenience CRDs offer (free declarative API, watch loop, RBAC, `kubectl`) is real but is *tooling*, not architecture, and is outweighed by the coupling for a component whose entire job is to operate the cluster from the outside.

## Appendix C — Prior art

| System | Pattern | Relation to Medea |
| --- | --- | --- |
| **Sidero Omni** | External standalone service, COSI runtime, declarative cluster templates, connects to machines over gRPC/SideroLink. Not in-cluster. | The shape we are building toward — Medea is "own-the-stack mini-Omni." |
| **Cluster API (CAPI)** | CRDs in a management K8s cluster; providers reconcile infra/bootstrap/control-plane. | Rejected (Appendix B); the management cluster is still K8s. |
| **system-upgrade-controller** | In-cluster `Plan` CRDs; agent DaemonSet drains + upgrades nodes. | Right *operation* (rolling upgrade), wrong *placement* (in-cluster). |
| **`talosctl`** | Imperative client to the Talos machine API. | The primitive Medea orchestrates; Medea adds store + reconcile + safety. |
| **EKS / AKS / GKE** | Fully external managed control plane in the provider's infra. | The ergonomic target; Medea is the self-hosted, owned analogue. |
