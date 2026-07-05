#!/usr/bin/env bash
# SKELETON — Phase-B bootstrap rehearsal (design/cluster-bootstrap.md §10, B-M3).
#
# Exercises the WHOLE Medea-driven cluster-create flow on a throwaway QEMU VM,
# so we never first rehearse on the Beelinks:
#
#   lab up  ->  medea serve --bootstrap  ->  medea cluster create --confirm
#   ->  CP VM PXE-boots (Medea-staged profile)  ->  Talos installs + reboots
#   ->  medea bootstraps etcd -> apiserver healthy -> Ready (seeds inventory)
#   ->  make bootstrap-cni (helm install Cilium)  ->  node Ready
#   ->  assert  ->  teardown
#
# Builds on lab.sh (bridge + dnsmasq + matchbox + PXE QEMU nodes). Linux-native;
# on macOS run it the same way lab.sh is run (inside the OrbStack Linux machine,
# see run.sh) so medea, matchbox and the VM share one network.
#
# STATUS: skeleton. The command sequence is real; three items need real runs to
# settle (they are flagged BLOCKER/NOTE inline and in README.md):
#   1. BLOCKER iPXE-HTTPS: Medea's BootAssets returns factory.talos.dev HTTPS URLs
#      that the lab's iPXE cannot fetch. Until Medea mirrors kernel/initrd through
#      matchbox HTTP (SESSION-HANDOFF §2a), AwaitingInstall never clears. lab.sh's
#      stage-stock shows the mirroring pattern.
#   2. NOTE CP fixed IP: --cp-ip must match the address the node actually gets.
#      Add a dnsmasq reservation (dhcp-host=<mac>,<ip>) for the CP MAC — lab.sh
#      does not pin one yet.
#   3. NOTE boot order: after install the node must boot the DISK, not PXE again.
#      Use disk-first + network-fallback (mirrors the Beelink BIOS recommendation);
#      lab.sh boot currently forces -boot n.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
LAB="$HERE/lab.sh"

# --- knobs ---------------------------------------------------------------------
CLUSTER="${CLUSTER:-dap}"
CP_MAC="${CP_MAC:-52:54:00:00:00:22}"
CP_IP="${CP_IP:-10.66.0.50}"                 # must be a dnsmasq reservation for CP_MAC (NOTE 2)
CP_ENDPOINT="https://${CP_IP}:6443"
TALOS_VERSION="${TALOS_VERSION:-v1.13.5}"
K8S_VERSION="${K8S_VERSION:-v1.36.1}"
INSTALL_DISK="${INSTALL_DISK:-/dev/vda}"     # QEMU virtio disk
HOME_CLUSTER="${HOME_CLUSTER:-$HERE/../../../home-cluster}"  # for `make bootstrap-cni`

WORKDIR="${WORKDIR:-/tmp/medea-netboot}"
MEDEA_HOME="$WORKDIR/medea"
MEDEA_ADDR="${MEDEA_ADDR:-127.0.0.1:7600}"
MEDEA_TOKEN="${MEDEA_TOKEN:-lab-token}"
export MEDEA_ADDR MEDEA_TOKEN

# NOTE 4: the whole-bringup timeout (install+reboot+etcd+apiserver) is a 30m const
#         in the reconciler (no serve flag). Under TCG (M1/M2, no nested KVM) install
#         alone can approach that — run this on a KVM host/CI, or make the timeout a
#         serve flag before rehearsing under TCG.

log() { echo ">> $*" >&2; }

# --- phases --------------------------------------------------------------------

start_medea() {
  mkdir -p "$MEDEA_HOME"
  printf '%s' "$MEDEA_TOKEN" >"$MEDEA_HOME/token"
  log "starting medea serve (--bootstrap; matchbox=$(matchbox_url))"
  # tls-cert/key self-sign when missing; creds-backend defaults to file. Runs on
  # the lab host so the reconciler can reach the CP Talos API over the bridge.
  go run ./cmd/medea serve \
    --listen "0.0.0.0:7600" \
    --store "$MEDEA_HOME/medea.db" \
    --token-file "$MEDEA_HOME/token" \
    --tls-cert "$MEDEA_HOME/cert.pem" --tls-key "$MEDEA_HOME/key.pem" \
    --creds-backend file --creds-dir "$MEDEA_HOME/creds" \
    --snapshot-dir "$MEDEA_HOME/snapshots" \
    --matchbox-dir "$WORKDIR/matchbox" --matchbox-url "$(matchbox_url)" \
    --install-disk "$INSTALL_DISK" \
    --provisioning --bootstrap \
    >"$MEDEA_HOME/serve.log" 2>&1 &
  echo $! >"$MEDEA_HOME/medea.pid"
  # TODO: the CLI must trust the self-signed cert — point its CA at $MEDEA_HOME/cert.pem
  #       (MEDEA_CACERT or equivalent). Confirm the client flag/env name.
  sleep 3
}

matchbox_url() { echo "http://$("$LAB" ip):8080"; }

create_cluster() {
  log "medea cluster create $CLUSTER (--cni none --disable-kube-proxy --confirm)"
  # Requires the CNI typed option (crunchloop/medea#1). Cilium itself is NOT passed
  # here — it is installed post-bootstrap by `make bootstrap-cni`.
  go run ./cmd/medea cluster create "$CLUSTER" \
    --cp-endpoint "$CP_ENDPOINT" --cp-mac "$CP_MAC" --cp-ip "$CP_IP" \
    --talos-version "$TALOS_VERSION" --kubernetes-version "$K8S_VERSION" \
    --install-disk "$INSTALL_DISK" \
    --cni none --disable-kube-proxy \
    --confirm
}

boot_cp() {
  log "PXE-booting the control-plane VM ($CP_MAC)"
  # NOTE 3: disk-first + network-fallback so the post-install reboot boots the disk.
  #         lab.sh boot forces -boot n; extend it (e.g. BOOT_ORDER=cn) before this works.
  BOOT_ORDER="${BOOT_ORDER:-cn}" "$LAB" boot "$CP_MAC" cp
}

wait_ready() {
  log "waiting for ClusterBootstrap $CLUSTER -> READY"
  # TODO: poll the bootstrap phase. Options: a `cluster get`/watch RPC (add one), or
  #       re-run `cluster create` (idempotent read of the record) and grep the phase.
  #       On BLOCKER 1 this parks at AWAITING_INSTALL — check $MEDEA_HOME/serve.log.
  echo "   (skeleton: implement the phase poll)"
}

install_cni() {
  log "fetching kubeconfig + helm-installing Cilium (make bootstrap-cni)"
  go run ./cmd/medea get credentials --cluster "$CLUSTER" --kubeconfig >"$MEDEA_HOME/kubeconfig"
  KUBECONFIG="$MEDEA_HOME/kubeconfig" make -C "$HOME_CLUSTER" bootstrap-cni
}

assert_ready() {
  log "asserting node Ready + Cilium running"
  export KUBECONFIG="$MEDEA_HOME/kubeconfig"
  kubectl wait --for=condition=Ready node --all --timeout=5m
  kubectl -n kube-system rollout status ds/cilium --timeout=5m
  kubectl get nodes -o wide
  log "REHEARSAL PASSED"
}

teardown() {
  log "teardown"
  [ -f "$MEDEA_HOME/medea.pid" ] && kill "$(cat "$MEDEA_HOME/medea.pid")" 2>/dev/null || true
  "$LAB" down 2>/dev/null || true
}

# --- dispatch ------------------------------------------------------------------
case "${1:-all}" in
all)
  trap teardown EXIT
  "$LAB" up
  start_medea
  create_cluster
  boot_cp
  wait_ready
  install_cni
  assert_ready
  ;;
up)            "$LAB" up ;;
medea)         start_medea ;;
create)        create_cluster ;;
boot)          boot_cp ;;
wait)          wait_ready ;;
cni)           install_cni ;;
assert)        assert_ready ;;
down)          teardown ;;
*) echo "usage: $0 {all|up|medea|create|boot|wait|cni|assert|down}" >&2; exit 2 ;;
esac
