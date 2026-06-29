# Aggregate: Host (provisioning inventory)

**Context:** Cluster Inventory (supporting) · **Type:** aggregate root ·
**Status:** Partially implemented (v2-M1) — registration/listing shipped; the
lifecycle reconciler (state beyond `Registered`, selector/replicas matching) lands
in v2-M3.

`Host` is a piece of bare metal Medea knows about *before* it becomes a cluster
member — the inventory the v1 model lacked (`Machine` only ever described nodes
that were already in a cluster). It is the foundation of the provisioning plane
(`design/provisioning-plane.md`).

- **Type:** `pb.Host` ([`gen/medea/v1`](../../gen/medea/v1))
- **Registered/removed by:** `server.RegisterHost` / `DeregisterHost` ([`internal/server`](../../internal/server))
- **Listed by:** `server.ListHosts`
- **Will be driven by:** the provisioning reconciler (v2-M3) for state beyond `Registered`

## Consistency boundary

- **One record per `cluster/mac`**, stored in `bDesired/hosts`
  (`store.PutHostDesired`/`GetHost`, key = `cluster\x00mac`). Identity is the NIC
  **MAC** (Matchbox groups key on it).
- **Desired/precious, written via CAS** (like `Cluster`/`NodePool`/`Machine`).
  Unlike `Machine`, a Host has **no observed projection** — it is pure desired
  inventory until the reconciler begins writing lifecycle state.
- Included in `Export`/`Import` (it is precious, unrebuildable operator intent).

## Lifecycle / state machine

`HostState` (`design/provisioning-plane.md` §2):

```
  Registered ──▶ Allocated ──▶ Provisioning ──▶ Ready
       ▲                            │
       └──── Deprovisioning ◀───────┴──▶ Failed
```

**Driven states (v2-M3 scale-out):** `Registered` → `Provisioning` (the
reconciler stages the host's boot config) → `Ready` (the node joined; a `Machine`
is bound and added to the pool's members). `Allocated` is defined but currently
skipped (the reconciler goes straight to `Provisioning` when it stages).
`Deprovisioning` (scale-in) and `Failed` (provision timeout) land in v2-M4.

## Invariants

| # | Invariant | Enforced by | Why |
| --- | --- | --- | --- |
| I1 | **Identity is the MAC; register requires cluster + mac.** | `store` key (`cluster\x00mac`) + `RegisterHost`/`PutHostDesired` validation (`InvalidArgument`) | A host needs a stable pre-boot identity; the MAC is the one thing known before it has an address (`provisioning-plane.md` §2). |
| I2 | **A host can only be registered into an existing cluster.** | `RegisterHost` (`NotFound` if the cluster is absent) | Inventory is per-cluster; a host must belong to a known cluster. |
| I3 | **Role defaults from the pool when omitted.** | `RegisterHost` (reads `NodePool.role` when `--role` is unset) | Operator convenience; the host's role should match the pool it will join. |
| I4 | **Re-registering is idempotent and does not clobber a reconciler-advanced lifecycle state** (e.g. `Ready` is not reset to `Registered`). | `RegisterHost` (upsert preserves a non-`UNSPECIFIED` existing `state` + `addr`) | Registration must be safe to re-run without undoing provisioning progress. Confirmed by `TestRegisterListDeregisterHost`. |
| I5 | **Host desired is persisted, CAS-guarded, and exported; it has no observed projection.** | `store.PutHostDesired` (CAS) + `Export`/`Import` include hosts; no `SetHostObserved` | Inventory is precious operator intent, not a rebuildable cache. Confirmed by `TestHostRoundTripCASListDelete` / `TestExportImportIncludesHosts`. |

## Command surface

| Command (gRPC) | CLI | Effect |
| --- | --- | --- |
| `RegisterHost` | `medea host register --cluster … --mac … [--pool --role --label k=v]` | Upsert a host as `Registered` (I1–I4). |
| `ListHosts` | `medea host list --cluster … [--pool …]` | Read inventory (optionally filtered by pool). |
| `DeregisterHost` | `medea host deregister --cluster … --mac …` | Remove a host from inventory. |

## Event surface

- `host` — emitted on register (write) and deregister (delete); consumed by `Watch`.

## Cross-context dependencies

- **Cluster Inventory:** registration validates the [`Cluster`](cluster.md) and
  (when a pool is given) the [`NodePool`](nodepool.md) for role defaulting. v2
  adds `NodePool.selector` matched against `Host.labels`.
- **Version Rollout / Talos / Kube:** none yet. In v2-M3 the provisioning
  reconciler will bind a [`Machine`](machine.md) to a `Ready` host and drive the
  lifecycle via the Matchbox driver.
- **Persistence + Shared Kernel:** `store.Store`; `pb.Host` is kernel.

## Key decisions

- **MAC-keyed inventory** (`provisioning-plane.md` §1 #3) — the one identity
  available before a host has booted/joined.
- **No observed projection** — a Host is desired inventory; its eventual `addr`
  and `Ready` state are written by the reconciler as desired-side lifecycle, not
  as a refreshed observed cache.
- **Secrets capture is done in v2-M1** (`talos.CaptureSecrets` +
  `creds.PutSecrets`, `medea capture-secrets`): the existing cluster's
  secrets bundle is extracted from a live control-plane config into the
  `CredentialStore` (`provisioning-plane.md` §5), ready for v2-M2 join-config
  generation.
- **Known gaps (after v2-M3):** scale-out is driven (Registered→Provisioning→
  Ready, replicas/selector matching, Matchbox staging); still pending —
  scale-in/`Deprovisioning`, a provision-timeout→`Failed` transition, and the
  serve-loop executor that runs the reconciler on an interval (v2-M4). End-to-end
  (real Matchbox + node boot) is validated on the QEMU/Beelink tier.
