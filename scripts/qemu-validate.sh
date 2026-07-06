#!/usr/bin/env bash
# Faithful end-to-end rollout validation on a real QEMU Talos cluster — the
# bare-metal A/B UpgradeOS path the docker provisioner can't test
# (design/talos-client.md §9).
#
# Portable across any host with talosctl + QEMU and hardware acceleration:
#   - Linux dev box OR self-hosted CI runner: needs /dev/kvm (nested virt or bare
#     metal), qemu, and iptables. talosctl auto-installs the CNI bundle to
#     /opt/cni/bin on first run — no manual CNI setup.
#   - macOS (Apple Silicon): needs QEMU (`brew install qemu`) + vmnet.
#
# The QEMU provisioner needs root (vmnet on macOS; bridge/iptables/KVM on Linux),
# so cluster create/destroy run privileged. Privilege is abstracted via $SUDO:
# empty when already root (CI runners), else `sudo` (a dev box prompts once). CI
# on a non-root runner therefore needs passwordless sudo. The Go test itself runs
# unprivileged.
#
# Usage:   ./scripts/qemu-validate.sh
# Tunables (env): NAME, TARGET, CPIP, WORKERIP, SUDO, KEEP.
#
# This NEVER touches the live production cluster — it builds its own throwaway VMs.
set -euo pipefail

NAME="${NAME:-medea-qemu}"
TARGET="${TARGET:-v1.13.5}"
DIR="$(mktemp -d)"
CPIP="${CPIP:-10.5.0.2}"          # qemu default cidr 10.5.0.0/24 -> first control plane
WORKERIP="${WORKERIP:-10.5.0.3}"  # ... second node

OS="$(uname -s)"

# Privilege escalation: none if already root (CI runners), else sudo. Override by
# exporting SUDO explicitly (SUDO="" to force none, SUDO="doas", …).
if [ "${SUDO+set}" != "set" ]; then
  if [ "$(id -u)" -eq 0 ]; then SUDO=""; else SUDO="sudo"; fi
fi

cd "$(dirname "$0")/.."

die() { echo ">> ERROR: $*" >&2; exit 1; }

# --- preflight ------------------------------------------------------------
command -v talosctl >/dev/null 2>&1 || die "talosctl not found on PATH"

case "$(uname -m)" in
  x86_64|amd64)  QEMU_BIN=qemu-system-x86_64 ;;
  arm64|aarch64) QEMU_BIN=qemu-system-aarch64 ;;
  *)             QEMU_BIN="" ;;
esac
if [ -n "$QEMU_BIN" ] && ! command -v "$QEMU_BIN" >/dev/null 2>&1; then
  echo ">> WARNING: $QEMU_BIN not on PATH; talosctl will need it to boot VMs." >&2
fi

if [ "$OS" = "Linux" ]; then
  # Without KVM, QEMU falls back to TCG software emulation — a full A/B OS
  # upgrade + reboot is then far too slow to finish in the test budget.
  [ -e /dev/kvm ] || die "/dev/kvm missing — this test needs KVM (nested virt / bare metal); TCG emulation is too slow."
fi

echo ">> os=$OS  cluster=$NAME  target=$TARGET  workdir=$DIR  sudo='${SUDO:-<none>}'"

# --- cleanup --------------------------------------------------------------
echo ">> destroying any prior cluster"
$SUDO talosctl cluster destroy --name "$NAME" 2>/dev/null || true

# `cluster destroy` only knows the cluster still in its state dir; a prior run
# that died mid-create (or was Ctrl-C'd) leaves ORPHANED root procs — the
# per-node QEMU, `talosctl qemu-launch`, `talosctl loadbalancer-launch`, and
# especially the talosctl dhcpd holding UDP/67. The next create then fails with
# "DHCPd server has not started" / "cannot bind to port 67: address already in
# use". Reap any "$NAME" orphans, then the stale state dir, before creating.
echo ">> reaping orphaned $NAME procs + stale state"
$SUDO pkill -f "$NAME" 2>/dev/null || true
$SUDO pkill -f "talosctl qemu-launch" 2>/dev/null || true
$SUDO pkill -f "talosctl loadbalancer-launch" 2>/dev/null || true
$SUDO rm -rf "$HOME/.talos/clusters/$NAME" 2>/dev/null || true

# macOS vmnet can leave `bootpd` holding UDP/67 (a vmnet-shared race); name it so
# the failure is diagnosable rather than a bare timeout. Linux talosctl runs its
# own dhcpd, covered by the reap above.
if [ "$OS" = "Darwin" ] && $SUDO lsof -nP -iUDP:67 >/dev/null 2>&1; then
  echo ">> WARNING: UDP/67 still held after cleanup — talosctl dhcpd will fail to bind:" >&2
  $SUDO lsof -nP -iUDP:67 >&2 || true
fi

# --- create ---------------------------------------------------------------
echo ">> creating QEMU cluster (downloads images + boots VMs, a few minutes)"
# disk-image preset: boot a pre-installed Talos disk (A/B partitions present, no
# install-then-reboot during bootstrap) — converges faster and is the right shape
# for testing an A/B OS upgrade. (The default 'iso' preset installs first and can
# time out bootstrap on macOS/QEMU.)
# Memory: the default 2 GiB is too tight for an in-place upgrade (pull
# metal-installer, run it, A/B install, reboot) — the node OOMs and never
# returns. Worker -> 4 GiB. The control-plane node carries etcd + apiserver +
# controllers ON TOP of the upgrade, so it needs more headroom -> 6 GiB;
# otherwise the control-plane upgrade hangs forever at "host is down".
$SUDO talosctl cluster create qemu --name "$NAME" --workers 1 \
  --presets disk-image \
  --memory-workers 4096 \
  --memory-controlplanes 6144 \
  --talosconfig-destination "$DIR/talosconfig"

# Files written by root -> make them readable by the unprivileged test.
$SUDO chown -R "$(id -u):$(id -g)" "$DIR"

echo ">> fetching kubeconfig (node $CPIP)"
talosctl --talosconfig "$DIR/talosconfig" kubeconfig --force --nodes "$CPIP" "$DIR/kubeconfig"

echo ">> running rollout validation (worker, then control-plane -> $TARGET)"
# -run TestQemuUpgrade matches both TestQemuUpgrade (worker) and
# TestQemuUpgradeControlPlane (the CP reboot / resume-after-reboot path), run in
# source order against this one cluster.
set +e
MEDEA_QEMU_TALOSCONFIG="$DIR/talosconfig" \
MEDEA_QEMU_KUBECONFIG="$DIR/kubeconfig" \
MEDEA_QEMU_TARGET="$TARGET" \
  go test -tags integration -run TestQemuUpgrade -timeout 75m -v ./test/e2e/
rc=$?
set -e

if [ "${KEEP:-0}" = "1" ] && [ "$rc" -ne 0 ]; then
  echo ">> KEEP=1 and test failed: leaving cluster up for inspection."
  echo "   cp:       $SUDO talosctl --talosconfig $DIR/talosconfig -n $CPIP dmesg"
  echo "   worker:   $SUDO talosctl --talosconfig $DIR/talosconfig -n $WORKERIP dmesg"
  echo "   k8s:      KUBECONFIG=$DIR/kubeconfig kubectl get nodes -o wide"
  echo "   destroy:  $SUDO talosctl cluster destroy --name $NAME"
  exit $rc
fi

echo ">> destroying cluster"
$SUDO talosctl cluster destroy --name "$NAME"

rm -rf "$DIR"
exit $rc
