# Datastore

**Status:** Draft for review
**Date:** 2026-06-25

Scope: the embedded store that holds Medea's state — schema, persistence,
revisions, watch, concurrency, crash recovery, and the desired-state export.
Implements PRD §13 decisions #7 (embedded), #13 (observed is a rebuildable
cache; only desired + in-flight rollout state is precious) and #8 (the stored
types *are* the gRPC types). Blocks milestone **M1** (skeleton + state
seeding).

Out of scope: the gRPC service definition and auth (→ `api-and-auth.md`); the
Talos/kube clients that populate observed (→ `talos-client.md`); credential
storage (see §9 — explicitly *not* in this store).

## 1. Technology: bbolt

A single-file, pure-Go, ACID embedded key/value store
(`go.etcd.io/bbolt`). One read-write transaction at a time, many concurrent
readers (MVCC), crash-safe by design (the file is always consistent after an
unclean shutdown).

Why bbolt over the alternatives:

- **vs SQLite (modernc pure-Go):** our access is pure key lookups and small
  prefix scans (by cluster / pool / node) over a tiny dataset (one cluster,
  a handful of nodes). We need no SQL, no query planner. bbolt is simpler and
  has no cgo. SQLite's relational power would be unused weight.
- **vs Postgres:** rejected at PRD level — it adds a runtime dependency, and
  Medea must not depend on infrastructure (least of all anything resembling
  the cluster it manages).
- **vs COSI runtime:** the maximally Talos-native option, kept on the table
  for a future migration (§10), but heavier to adopt now. bbolt gets us to
  M1 fastest with no conceptual debt we can't unwind.

The on-disk file (`medea.db`) lives wherever Medea runs (PRD §13 #10), not in
the cluster.

## 2. What is precious, what is not

Per PRD §13 #13, the store holds three classes of data with different
durability requirements:

| Class | Persisted? | Rebuildable? | Examples |
| --- | --- | --- | --- |
| **Desired** | yes — the precious data | no (only exists in Medea) | target Talos/K8s versions, pool membership, rollout strategy |
| **Rollout progress** | yes — precious *during* a rollout | no (resume needs it) | per-node `rollout.state`, cluster rollout phase |
| **Observed** | no (in-memory only) | yes — re-read from Talos/kube | current versions, node health/Ready |

Consequence: **observed is never written to bbolt.** It is held in an
in-memory projection rebuilt on boot by a refresh pass. This keeps the
persisted footprint tiny and makes the backup story (§7) trivial. (A
last-known observed *cache* could be persisted later purely to make the
read-path show data before the first refresh completes — explicitly a
display optimization, never trusted. Deferred.)

## 3. Schema (bucket layout)

bbolt is nested buckets of byte keys → byte values. Layout:

```
medea.db
├── meta/
│   ├── revision            → uint64 (global monotonic; bumped every write)
│   └── schema_version      → uint32
├── desired/
│   ├── clusters/<cluster>                 → Cluster   (desired fields only)
│   ├── nodepools/<cluster>/<pool>         → NodePool  (desired + strategy)
│   └── machines/<cluster>/<addr>          → Machine   (identity/membership)
└── rollouts/
    ├── clusters/<cluster>                 → ClusterRollout (K8s-path phase)
    └── machines/<cluster>/<addr>          → MachineRollout (OS-path state)
```

- Keys are hierarchical strings (`<cluster>/<pool>`), so a pool's nodes are a
  cheap prefix scan and everything is namespaced by cluster (multi-cluster
  ready without schema change).
- There is **no `observed/` bucket** — by design (§2).
- `meta/revision` is the single global counter (see §4).

## 4. Records, revisions, encoding

**Encoding — the stored bytes are the gRPC message.** The same Protobuf
messages the API speaks are serialized (proto wire format) and stored as the
bucket value. One `.proto` is the single source of type truth for the API,
the store, and the CLI — no separate "storage struct" layer to keep in sync.
Proto's field-level forward/backward compatibility gives us schema evolution
for free; `meta/schema_version` guards the rare breaking change.

**Revisions.** Every successful write bumps `meta/revision` and stamps the
record's `revision` field with the new value. This gives us, with one
mechanism:

- **Watch cursors** (§5): "give me everything since revision N."
- **Optimistic concurrency** (§6): compare-and-swap on a record's revision.

A write is: open RW txn → read current record (for CAS check) → bump
`meta/revision` → write record with new revision → commit → (post-commit)
publish the change to watchers (§5).

## 5. Watch

The gRPC read path needs streaming (`medea rollout status -w`, future UI).
bbolt has no native watch, so Medea runs an **in-process broadcaster**:

- After a write txn commits, the store publishes `{key, kind, revision}` to
  all subscribers.
- A subscriber opens with a `since` revision. The store first sends a
  **consistent snapshot** of matching records (read txn at current revision),
  then switches to the **live event stream** — no gap, because the snapshot's
  revision is the stream's starting cursor.
- The event log is **not durably persisted.** It doesn't need to be:
  watchers are in-process consumers (reconcilers) or reconnecting clients
  that re-snapshot on reconnect, and observed is rebuildable anyway. This
  keeps watch a memory-only concern.

This is deliberately modest — single-process, single-store, homelab scale. No
distributed watch, no compaction.

## 6. Concurrency

- **One writer at a time** (bbolt's RW-txn lock). Writers are few: the
  reconcilers and the API handlers. Reads are unconstrained (MVCC).
- **Writer separation reduces conflict to near-zero:** the **API** writes the
  `desired/` buckets; the **reconcilers** write the `rollouts/` buckets and
  the in-memory observed. They touch disjoint key spaces almost always.
- **Optimistic concurrency on desired writes:** API mutations are
  compare-and-swap on the record's `revision` (the CLI read-modify-write
  carries the revision it read). A stale write is rejected, not silently
  clobbered — so an operator `medea upgrade` can't stomp a concurrent change.
  Reconciler writes to `rollouts/` are last-writer-wins (single logical owner
  per record, so no contention).

## 7. Crash recovery & boot sequence

bbolt guarantees a consistent file after an unclean shutdown, so recovery is
not about file repair — it's about rebuilding the volatile parts:

```
on boot:
  1. open medea.db (bbolt) — desired/ and rollouts/ are immediately valid
  2. start refresh pass: for each cluster, read observed (versions, health)
     from Talos/kube → populate in-memory observed projection
  3. resume rollouts: for each rollouts/ record not in a terminal state,
     re-enter the rollout state machine at the recorded point
     (per rollout-controller.md §4 — every transition is idempotent)
  4. start API server + watch broadcaster
```

Step 3 is the concrete reason rollout progress is persisted (PRD §13 #14):
without it, a Medea restart mid-rollout would lose its place.

## 8. Store interface (Go sketch)

The store package exposes a typed, per-resource surface over the proto types
(`pb`). Representative, not exhaustive:

```go
type Revision uint64

type Store interface {
    // Desired (precious; API-owned; CAS writes)
    GetCluster(cluster string) (*pb.Cluster, Revision, error)
    PutClusterDesired(c *pb.Cluster, expected Revision) (Revision, error)
    ListNodePools(cluster string) ([]*pb.NodePool, error)
    PutNodePoolDesired(np *pb.NodePool, expected Revision) (Revision, error)
    ListMachines(cluster, pool string) ([]*pb.Machine, error)

    // Rollout progress (precious during rollout; reconciler-owned; LWW)
    GetMachineRollout(cluster, addr string) (*pb.MachineRollout, error)
    PutMachineRollout(r *pb.MachineRollout) error
    GetClusterRollout(cluster string) (*pb.ClusterRollout, error)
    PutClusterRollout(r *pb.ClusterRollout) error

    // Observed (in-memory cache; reconciler-owned; not persisted)
    SetObserved(cluster string, o *Observed)
    GetObserved(cluster string) (*Observed, bool)

    // Watch & backup
    Watch(ctx context.Context, since Revision) (<-chan Event, error)
    Export(w io.Writer) error            // desired-state only (§7 backup)
    Import(r io.Reader) error
}

type Event struct {
    Kind     string   // "cluster" | "nodepool" | "machine_rollout" | ...
    Key      string
    Revision Revision
}
```

Desired writes take an `expected Revision` (CAS); rollout/observed writes do
not (single owner). Observed is a plain in-memory map behind the same
interface so callers don't special-case it.

## 9. Desired-state export (the backup story)

Because desired state is the only precious-and-unrebuildable data and it is
tiny, backup is just an export of the `desired/` buckets:

- `Export` serializes all `desired/` records to a single JSON document
  (human-readable, diffable, git-friendly); `Import` restores them.
- Medea **auto-writes an export** (`medea-desired.json`) next to `medea.db`
  on every desired-state mutation. This is a **derived artifact for DR and
  inspection — never read at runtime as truth** (that would reintroduce the
  file-as-source-of-truth we rejected, PRD §13 #2). Runtime truth is always
  bbolt; the JSON is a one-way backup that a human or `medea import` can use
  to rebuild after total loss.

**Credentials are not in this store.** Talos admin certs (talosconfig) and
kubeconfig are sensitive and out of scope here; the `Cluster` record holds
only *references*, and the secret material lives in a separate mechanism
(OS keyring / mode-0600 file / external secret store) designed in
`api-and-auth.md`. Keeping secrets out of bbolt means the export in this
section is safe to commit/inspect.

## 10. Future work

- **COSI migration.** If we later want COSI's resource/finalizer/owner
  semantics (and closer Omni alignment), the typed `Store` interface is the
  seam: a COSI-backed implementation can replace the bbolt one without
  touching reconcilers. The proto types carry over.
- **Persisted observed cache** for instant read-path on cold start (§2) —
  display optimization only.
- **Rollout history / audit** — retain terminal `rollouts/` records (with
  timestamps injected at the API boundary, since the reconcile core is
  time-free) instead of clearing them, for "what did we upgrade and when."

## 11. Test plan (maps to PRD §9.1)

- **Round-trip:** put/get each desired and rollout record; proto bytes
  decode to equal messages.
- **Revisions:** every write bumps `meta/revision` monotonically; record
  revision matches.
- **CAS:** a `Put*Desired` with a stale `expected` revision is rejected; with
  the current revision succeeds.
- **Watch:** subscriber with `since=0` gets a full snapshot then live events;
  `since=N` gets only changes after N; no gap across the snapshot→live
  switch.
- **Crash recovery:** write desired + a mid-flight rollout record, reopen the
  store, assert both load intact and observed starts empty (rebuildable).
- **Export/import:** export → wipe → import reproduces the desired state;
  export contains no credential material.
