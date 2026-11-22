#!/usr/bin/env bash
# Build the Open Factory guest kernel (Linux 6.18) for Firecracker.
#
# Usage: build.sh <amd64|arm64> [output-dir]
#
# The script downloads the kernel source and the Firecracker CI config of
# the pinned versions, merges config-common and config-<arch> on top, and
# writes vmlinux-<version>-<arch> and its sha256 file to the output
# directory. x86_64 needs the uncompressed ELF vmlinux; aarch64 needs the
# PE "Image". Run it on a host of the same architecture, or set CROSS_COMPILE.
#
# Environment:
#   KERNEL_VERSION       Linux version (default 6.18.4)
#   FIRECRACKER_VERSION  Firecracker tag of the base config (default v1.18.0)
#   JOBS                 make -j value (default: the CPU count)
set -euo pipefail

arch="${1:-}"
out="${2:-$PWD/out}"
KERNEL_VERSION="${KERNEL_VERSION:-6.18.4}"
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.18.0}"
JOBS="${JOBS:-$(nproc)}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "$arch" in
  amd64) karch=x86_64; fcarch=x86_64; target=vmlinux; artifact=vmlinux ;;
  arm64) karch=arm64; fcarch=aarch64; target=Image; artifact=arch/arm64/boot/Image ;;
  *) echo "usage: $0 <amd64|arm64> [output-dir]" >&2; exit 2 ;;
esac

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$out"

major="${KERNEL_VERSION%%.*}"
echo "downloading linux-${KERNEL_VERSION}"
curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v${major}.x/linux-${KERNEL_VERSION}.tar.xz" -o "$work/linux.tar.xz"
tar -C "$work" -xf "$work/linux.tar.xz"
src="$work/linux-${KERNEL_VERSION}"

echo "downloading the Firecracker ${FIRECRACKER_VERSION} base config"
curl -fsSL "https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FIRECRACKER_VERSION}/resources/guest_configs/microvm-kernel-ci-${fcarch}-6.18.config" \
  -o "$src/.config"

(
  cd "$src"
  ARCH="$karch" ./scripts/kconfig/merge_config.sh -m .config "$here/config-common" "$here/config-${fcarch}"
  make ARCH="$karch" olddefconfig
  # merge_config.sh warns on dropped options; fail on the ones we need.
  for opt in VIRTIO_MMIO VIRTIO_BLK VIRTIO_NET VIRTIO_VSOCKETS HW_RANDOM_VIRTIO VMGENID EXT4_FS OVERLAY_FS DEVTMPFS SECCOMP_FILTER; do
    if ! grep -q "^CONFIG_${opt}=y" .config; then
      echo "CONFIG_${opt} is not built in" >&2
      exit 1
    fi
  done
  if grep -q '^CONFIG_MODULES=y' .config; then
    echo "CONFIG_MODULES must be off" >&2
    exit 1
  fi
  make ARCH="$karch" -j"$JOBS" "$target"
)

name="vmlinux-${KERNEL_VERSION}-${arch}"
cp "$src/$artifact" "$out/$name"
cp "$src/.config" "$out/$name.config"
(cd "$out" && sha256sum "$name" > "$name.sha256")
echo "built $out/$name"
