#!/usr/bin/env bash
# Install an Open Factory microVM worker host.
#
# The script installs openfactory-runtime, Firecracker and the jailer
# (v1.18.0) and the guest kernel, checks the SHA-256 of every file against
# the SHA256SUMS file of the Open Factory release, writes the
# openfactory-worker.service unit and runs worker doctor. It does not start
# the service. Next steps: worker enroll, then systemctl enable --now.
#
# Usage: sudo ./install.sh [--version <release>]
#
# Environment:
#   OF_VERSION      Open Factory release, for example 1.4.0 (default: latest)
#   OF_RELEASE_URL  base URL of the release files (default: GitHub releases)
set -euo pipefail

FIRECRACKER_VERSION="v1.18.0"
OF_VERSION="${OF_VERSION:-latest}"
REPO_URL="https://github.com/openfactory-org/openfactory"
DATA_DIR="/var/lib/openfactory"
CONFIG="/etc/openfactory/worker.yaml"
UNIT="/etc/systemd/system/openfactory-worker.service"

while [ $# -gt 0 ]; do
  case "$1" in
    --version) OF_VERSION="$2"; shift 2 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ "$OF_VERSION" = "latest" ]; then
  RELEASE_URL="${OF_RELEASE_URL:-$REPO_URL/releases/latest/download}"
else
  RELEASE_URL="${OF_RELEASE_URL:-$REPO_URL/releases/download/v$OF_VERSION}"
fi

fail() { echo "error: $*" >&2; exit 1; }
info() { echo "==> $*"; }

# 1. Platform.
[ "$(id -u)" -eq 0 ] || fail "run as root (sudo $0)"
[ "$(uname -s)" = "Linux" ] || fail "a worker host must run Linux"
case "$(uname -m)" in
  x86_64) arch=amd64; fcarch=x86_64 ;;
  aarch64) arch=arm64; fcarch=aarch64 ;;
  *) fail "unsupported CPU architecture $(uname -m); use amd64 or arm64" ;;
esac

# 2. Requirements.
if ! { exec 3<>/dev/kvm; } 2>/dev/null; then
  fail "no usable /dev/kvm. A worker needs hardware virtualization: use bare metal or a VPS plan with nested KVM. See docs/self-hosted-machines.md."
fi
exec 3>&-
[ -f /sys/fs/cgroup/cgroup.controllers ] || fail "cgroup v2 is required (boot with systemd.unified_cgroup_hierarchy=1)"
kernel="$(uname -r)"
kmajor="${kernel%%.*}"
krest="${kernel#*.}"
kminor="${krest%%.*}"
kminor="${kminor%%[!0-9]*}"
if [ "$kmajor" -lt 5 ] || { [ "$kmajor" -eq 5 ] && [ "$kminor" -lt 15 ]; }; then
  fail "host kernel $kernel is older than 5.15"
fi
missing=""
for tool in nft ip mkfs.ext4 git curl tar sha256sum systemctl; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
  echo "missing tools:$missing" >&2
  echo "  Debian/Ubuntu: apt-get install -y nftables iproute2 e2fsprogs git curl tar coreutils" >&2
  echo "  RHEL family:   dnf install -y nftables iproute e2fsprogs git curl tar coreutils" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# 3. Download and check.
fetch() { info "downloading $1"; curl -fsSL --retry 3 -o "$work/$2" "$1"; }
fetch "$RELEASE_URL/SHA256SUMS" SHA256SUMS
runtime="openfactory-runtime-linux-$arch"
fctgz="firecracker-$FIRECRACKER_VERSION-$fcarch.tgz"
kfile="$(grep -oE "vmlinux-[0-9.]+-$arch\$" "$work/SHA256SUMS" | head -n1 || true)"
[ -n "$kfile" ] || fail "the release lists no guest kernel for $arch"
fetch "$RELEASE_URL/$runtime" "$runtime"
fetch "$RELEASE_URL/$kfile" "$kfile"
fetch "https://github.com/firecracker-microvm/firecracker/releases/download/$FIRECRACKER_VERSION/$fctgz" "$fctgz"
info "checking checksums"
for f in "$runtime" "$kfile" "$fctgz"; do
  line="$(grep -E "[[:space:]]\*?$f\$" "$work/SHA256SUMS" || true)"
  [ -n "$line" ] || fail "SHA256SUMS has no entry for $f"
  (cd "$work" && echo "$line" | sha256sum -c --status) || fail "checksum mismatch for $f"
done

# 4. Install.
install -m 0755 "$work/$runtime" /usr/local/bin/openfactory-runtime
tar -C "$work" -xzf "$work/$fctgz"
fcdir="$DATA_DIR/bin/firecracker-$FIRECRACKER_VERSION"
install -d -m 0755 "$fcdir"
install -m 0755 "$work/release-$FIRECRACKER_VERSION-$fcarch/firecracker-$FIRECRACKER_VERSION-$fcarch" "$fcdir/firecracker"
install -m 0755 "$work/release-$FIRECRACKER_VERSION-$fcarch/jailer-$FIRECRACKER_VERSION-$fcarch" "$fcdir/jailer"
ksum="$(sha256sum "$work/$kfile" | cut -d' ' -f1)"
install -d -m 0755 "$DATA_DIR/kernels/$ksum"
install -m 0444 "$work/$kfile" "$DATA_DIR/kernels/$ksum/vmlinux"

# 5. Host settings.
cat > /etc/sysctl.d/90-openfactory.conf <<'CONF'
# Open Factory microVM worker: forward VM traffic (the nftables table
# openfactory decides what passes).
net.ipv4.ip_forward = 1
CONF
sysctl -q --load /etc/sysctl.d/90-openfactory.conf
echo tun > /etc/modules-load.d/openfactory.conf
modprobe tun 2>/dev/null || true

# 6. Configuration and unit.
install -d -m 0755 /etc/openfactory
if [ ! -f "$CONFIG" ]; then
  cat > "$CONFIG" <<'CONF'
# Open Factory microVM worker. worker enroll writes control_url and name.
slots: 2
slot: {vcpus: 2, memory_mib: 4096, scratch_gib: 40}
data_dir: /var/lib/openfactory
network:
  subnet: 10.213.0.0/16
snapshots: false
CONF
fi
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$here/openfactory-worker.service" ]; then
  install -m 0644 "$here/openfactory-worker.service" "$UNIT"
else
  fetch "$RELEASE_URL/openfactory-worker.service" openfactory-worker.service
  install -m 0644 "$work/openfactory-worker.service" "$UNIT"
fi
systemctl daemon-reload

# 7. Check.
info "running worker doctor"
/usr/local/bin/openfactory-runtime worker doctor --config "$CONFIG" || true

# 8. Next steps.
cat <<NEXT

Installed openfactory-runtime, Firecracker $FIRECRACKER_VERSION and the guest kernel.
Next:
  1. Fix every FAIL line of worker doctor above.
  2. sudo openfactory-runtime worker enroll --url https://<control-plane> --token <ofe_ token> --name <name>
  3. sudo systemctl enable --now openfactory-worker
NEXT
