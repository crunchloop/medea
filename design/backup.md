# Backup & restore (v3)

**Status:** Draft for review
**Date:** 2026-06-28

Scope: scheduled cluster backups and a guarded restore — the second Tier-2
pillar (PRD §6, §14). Builds on two v1 primitives: the etcd snapshot
(`talos.EtcdSnapshot`, used today by the pre-control-plane gate) and the
desired-state `Export`. v3 adds a backup *target* abstraction, a *schedule +
retention* loop, an *encrypted full-DR bundle*, and a *plan-then-confirm restore*
that v4 control-plane auto-repair will call.

Out of scope: provisioning (v2, `provisioning-plane.md`); auto-repair (v4); the
v1 ad-hoc pre-mutation snapshot stays as-is (this generalizes its destination).

## 1. Decisions (this pass — 2026-06-28)

1. **Destination = a pluggable `BackupTarget`** (ACL seam) with a local-dir impl
   and an object-store impl (S3/MinIO) first; 1Password/others later (§3).
2. **A backup is a full DR bundle: etcd snapshot + desired-state export +
   cluster secrets bundle** — everything to rebuild from zero (§4). This is the
   notable call: it puts **secrets in the backup**, which v1 deliberately kept
   out of `Export` (`api-and-auth.md` §5, `datastore.md` §9). Consequence:
   **the bundle is client-side encrypted** and the target is treated as holding
   secrets (§4.1). The regular desired-state auto-export stays secret-free and
   unchanged — the DR bundle is a *separate, encrypted* artifact.
3. **Schedule = fixed interval + keep-N** per cluster (§5). Cron/GFS tiers are
   future work.
4. **Restore = a guarded, plan-then-confirm primitive** (`RestoreEtcd`), exposed
   as a CLI command and callable programmatically by v4 auto-repair (§6).

## 2. Why this is small to build

The hard parts already exist:

- **Capture** — `talos.EtcdSnapshot(node, w)` streams a consistent snapshot; the
  v1 reconciler already calls it before control-plane mutations. v3 points the
  same stream at a `BackupTarget` instead of a local file, and adds the desired +
  secrets parts.
- **Desired-state** — `store.Export` already serializes the precious desired
  records to JSON.
- **Secrets** — already captured in the `CredentialStore` (v2 secrets capture, or
  v1 talosconfig/kubeconfig).

So v3 is mostly *orchestration* (schedule, retention, bundling, encryption) +ㅤthe
*restore* mechanism, not new cluster-facing primitives.

## 3. `BackupTarget` (ACL seam)

```go
// internal/backup — consumed by the backup reconciler; impls are the only place
// that import an object-store SDK (quarantine, like talos/k8supgrade).
type BackupTarget interface {
    Put(ctx context.Context, key string, r io.Reader) error
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    List(ctx context.Context, prefix string) ([]BackupRef, error)
    Delete(ctx context.Context, key string) error
}
```

- **v3 impls:** `local` (a directory; generalizes today's snapshot dir) and
  `s3` (S3/MinIO — off-box durability, the real DR value). A `1password` impl is
  future work (good for the small secret/desired parts, poor for large etcd
  blobs — so it would only ever hold the key or the small artifacts).
- Keys are namespaced: `<cluster>/<timestamp>/{etcd.snap.age,desired.json.age,secrets.age}`
  (or a single `bundle.age` tarball — see §4).
- Target config is **service config** (`medea.yaml`, like store/creds paths),
  not cluster desired state (`api-and-auth.md` §6).

## 4. The DR bundle

One backup = a bundle of three parts:

1. `etcd.snap` — the etcd snapshot (control-plane node).
2. `desired.json` — `store.Export` output (Cluster/NodePool/Machine/Host desired).
3. `secrets` — the cluster machine-secrets bundle from the `CredentialStore`
   (CA, tokens) + talosconfig/kubeconfig refs.

Bundled as a single tar, then encrypted (§4.1) → `bundle.age` written to the
target. (A single encrypted object is simpler to manage and restore atomically
than three separately-encrypted keys.)

### 4.1 Encryption + key escrow (the consequence of bundling secrets)

Because the bundle contains secrets, it is **client-side encrypted before it
leaves Medea** — the target never sees plaintext (so even an S3/MinIO bucket
compromise yields nothing). Plan:

- **Scheme:** `age` (X25519) — small, audited, no PKI. Medea encrypts to a
  configured recipient (public key); decryption needs the matching identity.
- **Key escrow — the chicken-and-egg:** the decryption identity must NOT live
  only on the Medea host, or losing that host loses both Medea *and* the means to
  read its backups. The identity is the operator's to **escrow out-of-band**
  (1Password — already the cluster's DR vault). Medea holds only the *public*
  recipient for writing; restore prompts for / loads the identity.
- This **supersedes** the v1 claim that credentials have no serialized form
  (`api-and-auth.md` §5, `datastore.md` §9) — *for this encrypted DR path only*.
  Those docs get an appendix pointer here. The plaintext desired-state
  auto-export remains secret-free.

## 5. Schedule + retention

Per-cluster config (desired state, on the `Cluster` record):

```
Cluster.backup = { intervalHours: 6, keepLast: 14, enabled: false }
```

- A **backup reconciler** (sibling of rollout/provision) runs on the interval:
  for each `backup.enabled` cluster whose last backup is older than
  `intervalHours`, take a bundle → `Put` → prune to `keepLast` (oldest first).
- `enabled` defaults **false** (same posture as `rolloutsEnabled` — nothing acts
  on a cluster until deliberately turned on).
- Records a `Backup` job/result (timestamp, size, target key, outcome) in the
  store — gives `medea backup list` and an audit trail (also seeds backup
  history, which rollouts lack today).
- Fixed-interval keeps v3 simple; cron + GFS retention tiers are future work.

## 6. Restore (guarded, plan-then-confirm)

Restore is the most destructive operation in the system — on single-member,
non-HA etcd it is effectively a **re-bootstrap from snapshot** (wipe etcd → bring
it back from the snapshot), which briefly destroys and recreates cluster state.
So it is gated hard:

```
medea restore --cluster home --backup <id|--latest>     # PLAN: shows snapshot
   age, target version, the node, and the recover steps; creates/does nothing.
medea restore --cluster home --backup <id> --confirm     # acts
```

- **Plan-then-confirm**, like upgrades, plus a **stronger confirmation** for
  restore (e.g. require typing the cluster name) given it wipes etcd.
- Gated by a per-cluster guard (restore refused unless the cluster is
  enabled for it), mirroring `rollout-safety.md`.
- **`RestoreEtcd` primitive** lives behind the talos client seam. Mechanism on
  single-member: fetch+decrypt the bundle, then drive Talos's etcd disaster
  recovery (`talosctl bootstrap --recover-from=<snapshot>` / the etcd recover
  flow). Exact symbols are **version-coupled** and pinned at impl time against
  the supported Talos release (like `upgrade-k8s`, `talos-client.md` §7) — see
  §9.
- **v4 reuse:** control-plane auto-repair calls the same `RestoreEtcd` primitive
  (after re-provisioning a dead CP node), which is *why* restore lands in v3
  before repair (PRD §13 #19).

## 7. Milestones (v3)

- **v3-M1 — Target + bundle.** `BackupTarget` seam + local & S3/MinIO impls;
  bundle assembly (etcd + desired + secrets) + `age` encryption; `medea backup
  now --cluster …`. Unit-tested with a fake target.
- **v3-M2 — Schedule + retention.** Backup reconciler (interval, keep-N,
  `enabled` guard); `Backup` records; `medea backup list`.
- **v3-M3 — Restore.** `RestoreEtcd` primitive (pinned Talos recover);
  plan-then-confirm `medea restore`; integration-validate a real
  snapshot→restore on the docker/QEMU tier (a throwaway cluster: snapshot →
  mutate → restore → assert state returns).

## 8. Prior art

| System | Shape | Relation |
| --- | --- | --- |
| **Talos etcd disaster recovery** | `talosctl etcd snapshot` + `bootstrap --recover-from`. | The exact primitive `RestoreEtcd` wraps. |
| **Velero** | K8s-native backup/restore to object storage, scheduled, with restic. | The ergonomic target (schedule + object store); Velero backs up K8s objects, we back up etcd+desired+secrets. |
| **etcdctl snapshot save/restore** | Raw etcd snapshot lifecycle. | What Talos wraps underneath. |
| **age / restic** | Client-side encryption / encrypted backup. | `age` is the chosen encryption (§4.1). |

## 9. Open questions

- **Exact Talos recover symbols/flow** (single-member, disk-image nodes) —
  pinned at v3-M3 against the supported Talos release; validate on QEMU first
  (never the live cluster).
- **Restore target** — recover in place on the existing CP node vs recover onto a
  freshly re-provisioned node (the v4 repair case). v3 targets in-place; the
  v2 provisioning seam makes the fresh-node path a v4 concern.
- **Single bundle vs separate objects** (§4) — tarball is simpler; separate
  objects allow desired-only restore. Lean tarball; revisit if partial restore
  is wanted.
- **Key rotation** for the `age` recipient (re-encrypt existing backups?).
- **Stronger restore confirmation** — type-the-cluster-name vs a `--yes-really`
  flag vs a time-boxed token.

## 10. Test strategy (maps to PRD §9)

- **Unit (fakes):** the backup reconciler (interval triggers, keep-N pruning,
  `enabled` guard) against a fake `BackupTarget`; bundle assembly +
  encrypt/decrypt round-trip (age); restore planning (refused without confirm).
- **Integration:** real `Put`/`Get` against MinIO; real etcd snapshot → target.
- **E2E (QEMU, pre-release):** snapshot a throwaway cluster → make a visible
  change → restore → assert the change is gone and the cluster is healthy. Never
  the live cluster.
