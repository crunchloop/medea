#!/usr/bin/env bash
# Faithful end-to-end rollout validation on a real QEMU Talos cluster — the
# bare-metal A/B UpgradeOS path the docker provisioner can't test
# (design/talos-client.md §9). Apple Silicon + QEMU (`brew install qemu`).
#
# QEMU on macOS needs root for vmnet, so the cluster create/destroy run under
# sudo (you'll be prompted once). The Go test itself runs unprivileged.
#
# Usage:   ./scripts/qemu-validate.sh
# Tunables (env): NAME (cluster name), TARGET (upgrade-to version).
#
# This NEVER touches the live production cluster — it builds its own throwaway VMs.
set -euo pipefail

NAME="${NAME:-medea-qemu}"
TARGET="${TARGET:-v1.13.5}"
DIR="$(mktemp -d)"
CPIP="${CPIP:-10.5.0.2}" # qemu default cidr 10.5.0.0/24 -> first control plane

cd "$(dirname "$0")/.."

echo ">> cluster=$NAME  target=$TARGET  workdir=$DIR"
echo ">> destroying any prior cluster (sudo)"
sudo talosctl cluster destroy --name "$NAME" 2>/dev/null || true

echo ">> creating QEMU cluster (sudo; downloads images + boots VMs, a few minutes)"
# disk-image preset: boot a pre-installed Talos disk (A/B partitions present, no
# install-then-reboot during bootstrap) — converges faster and is the right shape
# for testing an A/B OS upgrade. (The default 'iso' preset installs first and can
# time out bootstrap on macOS/QEMU.)
# 4 GiB worker: the default 2 GiB is too tight for an in-place upgrade (pull
# metal-installer, run it, A/B install, reboot) and the node OOMs / never returns.
sudo talosctl cluster create qemu --name "$NAME" --workers 1 \
  --presets disk-image \
  --memory-workers 4096 \
  --talosconfig-destination "$DIR/talosconfig"

# Files written by root under sudo -> make them readable by us.
sudo chown -R "$(id -u):$(id -g)" "$DIR"

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
  go test -tags integration -run TestQemuUpgrade -timeout 75m -v ./internal/itest/
rc=$?
set -e

if [ "${KEEP:-0}" = "1" ] && [ "$rc" -ne 0 ]; then
  echo ">> KEEP=1 and test failed: leaving cluster up for inspection."
  echo "   cp:       sudo talosctl --talosconfig $DIR/talosconfig -n $CPIP dmesg"
  echo "   worker:   sudo talosctl --talosconfig $DIR/talosconfig -n 10.5.0.3 dmesg"
  echo "   k8s:      KUBECONFIG=$DIR/kubeconfig kubectl get nodes -o wide"
  echo "   destroy:  sudo talosctl cluster destroy --name $NAME"
  exit $rc
fi

echo ">> destroying cluster (sudo)"
sudo talosctl cluster destroy --name "$NAME"

rm -rf "$DIR"
exit $rc
