#!/usr/bin/env bash
# Cross-platform launcher for the netboot lab. On Linux it runs lab.sh directly;
# on macOS it runs the SAME lab.sh inside an OrbStack Linux machine (one
# implementation, CI == local). The repo is visible inside the OrbStack machine
# at the same path (OrbStack mounts the macOS filesystem).
#
# On Apple Silicon M1/M2 there is no nested /dev/kvm, so QEMU inside the Linux
# machine runs under TCG (functional but slow). On a Linux CI runner with KVM it
# is fast. ARCH defaults to the host arch in each environment.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"

case "$(uname -s)" in
Linux)
  exec "$HERE/lab.sh" "$@"
  ;;
Darwin)
  MACHINE="${ORB_MACHINE:-medea-netboot}"
  if ! orb list 2>/dev/null | awk '{print $1}' | grep -qx "$MACHINE"; then
    echo ">> creating OrbStack Linux machine '$MACHINE'" >&2
    orb create ubuntu "$MACHINE" >&2
  fi
  # Pass HERE + args into the machine; run lab.sh there.
  exec orb run -m "$MACHINE" bash -c 'cd "$1"; shift; exec ./lab.sh "$@"' bash "$HERE" "$@"
  ;;
*)
  echo "unsupported OS: $(uname -s)" >&2
  exit 1
  ;;
esac
