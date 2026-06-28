# Auto-repair (v4)

**Status:** Draft for review
**Date:** 2026-06-28

Scope: detect a failed node and bring it back automatically — the third and last
Tier-2 pillar (PRD §6, §14). It is the riskiest feature in Medea (automated,
destructive action), so its design is mostly *restraint*: what counts as
"failed," what repair is allowed to do without a human, and how it degrades when
the hardware can't be power-cycled. Builds directly on v2 (reprovision) and v3
(`RestoreEtcd`), plus a new `Power` seam.

Out of scope: provisioning mechanics (v2, `provisioning-plane.md`); backup/restore
mechanics (v3, `backup.md`). This record covers detection, the repair
state-machine, the safety gates, and the `Power` interface.

## 1. Decisions (this pass — 2026-06-28)

1. **Detection = sustained unreachable, debounced.** A node is a repair
   candidate when it is both kube-`NotReady` *and* Talos-unreachable for a
   configurable threshold, debounced against transient blips (§3). Source: the
   existing observed-state refresh loop.
2. **Repair action = reprovision the same host** (§4). Worker: drain-if-reachable
   → deprovision → reprovision (v2) → rejoin. Control-plane: reprovision →
   `RestoreEtcd` (v3) → rejoin.
3. **Control-plane repair is semi-automatic** (§5): workers repair fully
   automatically (gated); a CP failure is *detected and a restore plan prepared*,
   but a human must `--confirm` the etcd restore. Re-bootstrapping single-member
   etcd is too destructive to fully automate.
4. **Power is a pluggable `Power` driver with graceful degradation** (§6):
   smart-plug/Redfish (true cycle → hands-off) | WoL (cleanly-off only) | none.
   Where no real power-cycle exists, repair *stages* the reprovision and
   *notifies a human to power-cycle* rather than pretending it can recover a hung
   node.

## 2. Why restraint is the whole design

Auto-repair holds live admin credentials and destructive primitives and acts
*without a human in the loop* — the exact thing `rollout-safety.md` was written to
prevent for upgrades. So it inherits that posture and adds more:

- **Default off**, per cluster (`autoRepairEnabled`), never set by seed; the
  global executor flag still applies. The live production cluster is simply never
  enabled.
- **One repair at a time per cluster**, with a **cooldown/backoff** between
  attempts and a **max-attempts** cap per host — so a crashlooping node can't
  drive a repair storm.
- **Never repair a node that's intentionally down** — detection is suppressed
  while that node has a rollout in flight (`MachineRollout` Upgrading/
  WaitingHealthy) or is mid-provisioning. An upgrade reboot is not a failure.
- **Control-plane needs a human** (§5).

## 3. Detection

A **detector** runs over the observed state the refresh loop already collects
(`refresh`, `datastore.md` §2):

```
candidate(node) :=
    observed.NotReady AND talos-unreachable
    for >= cluster.repair.threshold          (default ~10m, debounced)
    AND no MachineRollout in {Upgrading, WaitingHealthy} for the node
    AND host not in {Provisioning, Deprovisioning}
    AND cluster.autoRepairEnabled
```

- **Debounce**: the condition must hold across N consecutive refresh passes, not
  a single one — transient apiserver blips, planned reboots, and the brief
  unreachability during a rollout must not trip it.
- **CP-aware (optional, decision-1 alt):** for a control-plane node, etcd member
  health can sharpen "dead" vs "Ready-but-etcd-sick"; v4 starts with the
  reachability signal and treats etcd health as future refinement.

## 4. Repair action & state machine

A new **`RepairJob`** aggregate (sibling of `Rollout`), reconciler-owned:

```
RepairJob{ cluster, host (mac), node (addr), role, state, attempts, message, createdAt }

state:
  Detected ─▶ Pending ─▶ Draining ─▶ Reprovisioning ─▶ WaitingHealthy ─▶ Done
                 │           (worker)        │                │
                 │                           ▼                │
  (control-plane)└────────────────▶ AwaitingConfirm ─▶ Restoring ─▶ WaitingHealthy
                                       (human --confirm)   (RestoreEtcd, v3)
   any ─▶ Failed     (max attempts / timeout / unrecoverable; halts, surfaces)
```

- **Worker** (fully auto, gated): `Pending` → drain if reachable (reuse rollout's
  kube ops) → deprovision + reprovision via the v2 plane → wait-healthy → `Done`.
- **Control-plane** (semi-auto): → `AwaitingConfirm` with a prepared
  `RestoreEtcd` plan; on `medea repair confirm` (or `medea restore --confirm`) →
  reprovision → `Restoring` (v3) → wait-healthy → `Done`.
- **Reuses v1/v2/v3 wholesale:** drain/wait/halt-on-failure (rollout), the
  provisioning reconciler (v2), `RestoreEtcd` (v3). Auto-repair is the
  *orchestrator*, not new low-level primitives.

## 5. Control-plane: semi-automatic

Worker loss on this cluster is non-critical (drain + reprovision). Control-plane
loss on a **single-master, non-HA** cluster means etcd is gone and recovery is a
**re-bootstrap from snapshot** (`backup.md` §6) — irreversible if the wrong
snapshot or a split-brain is involved. So:

- Medea **detects** the CP failure, **prepares** the restore plan (which backup,
  which node, the steps), and **parks at `AwaitingConfirm`** with a clear,
  loud surfacing.
- A human runs the confirm; Medea then drives reprovision + `RestoreEtcd`.
- Rationale: same logic as "snapshot-before-control-plane is mandatory but plan/
  confirm is the gate" (`rollout-safety.md` §4) — the operator selects to
  proceed; the machine does the careful steps.

(Once the cluster is HA — a future provisioning capability — fully-automatic CP
repair becomes reasonable, because losing one of three etcd members is routine.
v4 targets the non-HA reality.)

## 6. The `Power` interface

```go
// internal/power — the v4 seam reserved since v2 (provisioning-plane.md §7).
type Power interface {
    Cycle(ctx context.Context, host *pb.Host) error // off then on (true recovery)
    On(ctx context.Context, host *pb.Host) error
    Off(ctx context.Context, host *pb.Host) error
}
```

Impls and what each enables:

| Impl | Capability | Repair autonomy |
| --- | --- | --- |
| **smart-plug / PDU** (Shelly/Tasmota, HTTP/MQTT) | true off→on cycle | fully hands-off, incl. a *hung* node |
| **Redfish** (if a BMC exists) | true cycle | fully hands-off |
| **WoL** | power *on* a cleanly-off node only | recovers crash-then-off; **cannot** recover a hung node |
| **none** | — | repair **stages** reprovision + **notifies** a human to power-cycle |

**Graceful degradation:** repair queries the configured `Power` impl's
capability. With a true cycle it completes hands-off; with WoL-only or none it
advances as far as it can (stage the Matchbox profile so the *next* boot
reprovisions) and parks with a notification — honest about the human step rather
than hanging. The concrete impl for the Beelinks (likely WoL or a smart plug)
is pinned after the operator's BIOS/WoL/BMC capability check (an open question
carried from `provisioning-plane.md` §7).

## 7. Surfacing / notification

v4 records the `RepairJob` and emits events (`repair_job`), so `medea repair
list|status` and `Watch` show in-flight repairs and `AwaitingConfirm` / degraded
states. An actual *push* notification (Slack/email/webhook) is **future work** —
the seam is the event stream; a notifier consumes it.

## 8. Milestones (v4)

- **v4-M1 — Detector + RepairJob.** Detection over observed state (debounce,
  suppression during rollout/provision), the `RepairJob` aggregate + store, the
  `autoRepairEnabled`/cooldown/max-attempts gates; `medea repair list|status`.
  Detect-and-record only (no action) — safe first increment.
- **v4-M2 — Worker auto-repair.** Wire the worker path (drain → reprovision via
  v2 → rejoin) behind the gates; the `Power` seam + WoL impl + degraded
  "stage+notify" mode. E2E on QEMU: kill a worker → auto-reprovision → rejoin.
- **v4-M3 — Control-plane semi-auto.** `AwaitingConfirm` + confirm flow driving
  reprovision + `RestoreEtcd` (v3); QEMU validation of a CP loss → guided
  recovery. Optional smart-plug `Power` impl for hands-off worker repair.

## 9. Prior art

| System | Shape | Relation |
| --- | --- | --- |
| **CAPI `MachineHealthCheck`** | Declares unhealthy conditions + timeouts; remediates by deleting/replacing the Machine. | The detection+remediation model; we reprovision-in-place rather than delete, and gate CP. |
| **Metal3 remediation** | BMC power-cycle → reprovision on host failure. | The power-cycle-then-reprovision flow; we degrade gracefully without a BMC. |
| **node-problem-detector** | Surfaces node conditions (no remediation). | A detection-signal source we could consume later. |
| **Sidero / Omni** | Talos-native machine lifecycle + health. | The Talos-native end-state; Omni does this externally (our shape). |

## 10. Open questions

- **Crashloop vs single failure** — how many reprovision attempts before giving
  up and forcing alert-only (max-attempts value)? Backoff curve?
- **Flapping** — a node that recovers on its own mid-repair: detect and abort the
  RepairJob cleanly.
- **Power impl for the Beelinks** — WoL vs smart-plug; pending the hardware check
  (`provisioning-plane.md` §7). Determines how hands-off worker repair really is.
- **etcd health as a CP signal** (§3) — worth adding for sharper CP detection?
- **Replacement vs in-place** — if reprovision-in-place keeps failing (bad
  hardware), fall back to "replace with a spare host"? (Ties to inventory having
  spares.)
- **Notification transport** (§7) — which sink(s) first.

## 11. Test strategy (maps to PRD §9)

- **Unit (fakes):** the detector (debounce across passes; suppression while a
  rollout/provision is in flight; `autoRepairEnabled`/cooldown/max-attempts
  gates); the RepairJob state machine (worker path, CP → AwaitingConfirm);
  graceful degradation when `Power` has no true cycle.
- **Integration:** worker repair against a scratch cluster + a fake `Power`.
- **E2E (QEMU, pre-release):** kill a worker VM → auto-detect → reprovision →
  rejoin; then a CP-loss → detect → AwaitingConfirm → guided
  reprovision+restore. Never the live cluster.
