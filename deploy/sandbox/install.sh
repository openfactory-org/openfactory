#!/bin/sh
# Installs the Open Factory sandbox: the toolchain, the claude and codex CLIs,
# the users agent (uid 1001) and ofproxy, the bootstrap, and (optional) a baked
# openfactory-runtime binary. Runs as root on Ubuntu 24.04 or Debian trixie.
# It is idempotent. Used by deploy/sandbox/Dockerfile (Daytona snapshot and the
# generic image), Dockerfile.cloudflare (through that image) and
# scripts/boat-snapshot.sh.
#
#   sh install.sh [--profile base|boat] [--claude-version V] [--codex-version V]
#                 [--runner PATH] [--go-version V] [--node-major N]
#
# Profiles:
#   base   No sudo, no setuid or setgid binaries.
#   boat   boat.dev saved snapshot. sudo stays (the boat user needs it), and
#          sudo is the only setuid binary. The agent user has no sudo rights.
#          Adds openfactory-netlock.service, which blocks the egress of the
#          agent user at boot (firewall rules are not kept in snapshots).
#
# Exit status: 0 when the install and the self-check pass, 1 otherwise.

set -eu

PROFILE=base
CLAUDE_VERSION=latest
CODEX_VERSION=latest
CLAUDE_PACKAGE=@anthropic-ai/claude-code
CODEX_PACKAGE=@openai/codex
RUNNER=
GO_VERSION=1.27.4
NODE_MAJOR=22
AGENT_UID=1001
PROXY_UID=65532
OF_DIR=/opt/openfactory
TOOLS=$OF_DIR/tools
SRC=$(cd "$(dirname "$0")" && pwd)

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}
log() { printf '==> %s\n' "$*"; }

while [ $# -gt 0 ]; do
	case $1 in
	--profile) PROFILE=${2:?}; shift 2 ;;
	--claude-version) CLAUDE_VERSION=${2:?}; shift 2 ;;
	--codex-version) CODEX_VERSION=${2:?}; shift 2 ;;
	--runner) RUNNER=${2:?}; shift 2 ;;
	--go-version) GO_VERSION=${2:?}; shift 2 ;;
	--node-major) NODE_MAJOR=${2:?}; shift 2 ;;
	-h | --help)
		sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $1" ;;
	esac
done

case $PROFILE in base | boat) ;; *) die "unknown profile: $PROFILE (use base or boat)" ;; esac
[ "$(id -u)" -eq 0 ] || die "run as root"
[ -z "$RUNNER" ] || [ -f "$RUNNER" ] || die "runner binary not found: $RUNNER"

ARCH=$(dpkg --print-architecture)
case $ARCH in amd64 | arm64) ;; *) die "unsupported architecture: $ARCH" ;; esac

export DEBIAN_FRONTEND=noninteractive

# ---------- 1. Packages ----------

log "packages"
apt-get update
# shellcheck disable=SC2086 # the word list is intended
apt-get install -y --no-install-recommends \
	ca-certificates curl wget gnupg git git-lfs openssh-client \
	build-essential pkg-config make unzip zip xz-utils less procps \
	python3 python3-pip python3-venv pipx \
	ripgrep jq fd-find tree tini iproute2 iptables nftables
command -v fd >/dev/null 2>&1 || ln -sf "$(command -v fdfind)" /usr/local/bin/fd

# ---------- 2. Node.js, gh, Go ----------

node_major() { node --version 2>/dev/null | sed -n 's/^v\([0-9]*\).*/\1/p'; }
if [ "$(node_major)" != "$NODE_MAJOR" ]; then
	log "node $NODE_MAJOR"
	mkdir -p /etc/apt/keyrings
	curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor --yes -o /etc/apt/keyrings/nodesource.gpg
	echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
		>/etc/apt/sources.list.d/nodesource.list
	apt-get update
	apt-get install -y --no-install-recommends nodejs
fi

if ! command -v gh >/dev/null 2>&1; then
	log "gh"
	mkdir -p /etc/apt/keyrings
	curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
	chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
	echo "deb [arch=$ARCH signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
		>/etc/apt/sources.list.d/github-cli.list
	apt-get update
	apt-get install -y --no-install-recommends gh
fi

if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null || true)" != "go$GO_VERSION" ]; then
	log "go $GO_VERSION"
	rm -rf /usr/local/go
	curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" | tar -C /usr/local -xz
fi

# ---------- 3. Harness CLIs ----------

log "claude ($CLAUDE_PACKAGE@$CLAUDE_VERSION) and codex ($CODEX_PACKAGE@$CODEX_VERSION)"
mkdir -p "$TOOLS/bin"
npm install --global --no-fund --no-audit --prefix "$TOOLS/claude" "$CLAUDE_PACKAGE@$CLAUDE_VERSION"
npm install --global --no-fund --no-audit --prefix "$TOOLS/codex" "$CODEX_PACKAGE@$CODEX_VERSION"
ln -sf "$TOOLS/claude/bin/claude" "$TOOLS/bin/claude"
ln -sf "$TOOLS/codex/bin/codex" "$TOOLS/bin/codex"
npm cache clean --force >/dev/null 2>&1 || true

cat >/etc/profile.d/openfactory.sh <<'EOF'
# Open Factory: the harness CLIs and Go come first on the PATH.
PATH=/opt/openfactory/tools/bin:/usr/local/go/bin:$PATH
export PATH
EOF
chmod 0644 /etc/profile.d/openfactory.sh

# ---------- 4. Users ----------

log "users agent ($AGENT_UID) and ofproxy ($PROXY_UID)"
if ! getent group agent >/dev/null; then groupadd -g "$AGENT_UID" agent; fi
if ! id -u agent >/dev/null 2>&1; then
	useradd -m -u "$AGENT_UID" -g agent -s /bin/bash -d /home/agent agent
fi
[ "$(id -u agent)" = "$AGENT_UID" ] || die "user agent has uid $(id -u agent), want $AGENT_UID"
if ! getent group ofproxy >/dev/null; then groupadd -g "$PROXY_UID" ofproxy; fi
if ! id -u ofproxy >/dev/null 2>&1; then
	useradd -r -M -u "$PROXY_UID" -g ofproxy -d /nonexistent -s /usr/sbin/nologin ofproxy
fi
# The agent is only in its own group: no sudo, docker, kvm, lxd, adm, wheel, disk.
usermod -G "" agent
mkdir -p /workspace
chown agent:agent /workspace

# ---------- 5. Hardening ----------

log "hardening ($PROFILE)"
rm -f /etc/sudoers.d/agent
if [ -d /etc/sudoers.d ]; then
	# No sudoers entry may name the agent.
	if grep -rlsE '^[[:space:]]*agent[[:space:]]' /etc/sudoers.d /etc/sudoers 2>/dev/null; then
		die "a sudoers file grants rights to agent"
	fi
fi
if [ "$PROFILE" = base ]; then
	if dpkg -s sudo >/dev/null 2>&1; then
		SUDO_FORCE_REMOVE=yes apt-get purge -y sudo
	fi
	keep=""
else
	keep=/usr/bin/sudo
	[ -d /home/user ] && chmod 0700 /home/user
fi
find / -xdev \( -perm -4000 -o -perm -2000 \) -type f 2>/dev/null | while read -r f; do
	[ "$f" = "$keep" ] && continue
	chmod u-s,g-s "$f"
done

# ---------- 6. Bootstrap and runner ----------

log "bootstrap"
mkdir -p "$OF_DIR/bin" /var/lib/openfactory
chmod 0755 "$OF_DIR" "$OF_DIR/bin"
chmod 0700 /var/lib/openfactory
install -o root -g root -m 0755 "$SRC/bootstrap.sh" "$OF_DIR/bootstrap.sh"
install -o root -g root -m 0755 "$SRC/trust-ca.sh" "$OF_DIR/trust-ca.sh"
if [ -n "$RUNNER" ]; then
	install -o root -g root -m 0755 "$RUNNER" "$OF_DIR/bin/openfactory-runtime"
fi
chown -R root:root "$OF_DIR"
chmod -R go-w "$OF_DIR"

# ---------- 7. boat: baseline lock at boot ----------

if [ "$PROFILE" = boat ]; then
	log "openfactory-netlock.service"
	cat >"$OF_DIR/netlock-baseline.sh" <<EOF
#!/bin/sh
# Baseline egress lock: the agent user reaches loopback only. The runner adds
# the session rules (internal/sandbox/netlock). Do not touch the boat user.
set -eu
for ipt in iptables ip6tables; do
	\$ipt -w 5 -C OUTPUT -m owner --uid-owner $AGENT_UID ! -o lo -j REJECT 2>/dev/null ||
		\$ipt -w 5 -I OUTPUT 1 -m owner --uid-owner $AGENT_UID ! -o lo -j REJECT
done
EOF
	chmod 0755 "$OF_DIR/netlock-baseline.sh"
	cat >/etc/systemd/system/openfactory-netlock.service <<EOF
[Unit]
Description=Open Factory baseline egress lock for the agent user
After=network-pre.target ufw.service
Before=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$OF_DIR/netlock-baseline.sh

[Install]
WantedBy=multi-user.target
EOF
	if command -v systemctl >/dev/null 2>&1; then systemctl enable openfactory-netlock.service; fi
fi

apt-get clean
rm -rf /var/lib/apt/lists/*

# ---------- 8. Self-check ----------

log "self-check"
fail=0
check() {
	if eval "$2" >/dev/null 2>&1; then
		printf '  PASS  %s\n' "$1"
	else
		printf '  FAIL  %s\n' "$1"
		fail=1
	fi
}
check "agent has uid $AGENT_UID" "[ \"\$(id -u agent)\" = $AGENT_UID ]"
check "agent is only in group agent" "[ \"\$(id -Gn agent)\" = agent ]"
check "ofproxy exists" "id -u ofproxy"
check "no sudoers file for agent" "[ ! -e /etc/sudoers.d/agent ]"
if [ "$PROFILE" = base ]; then
	check "sudo is not installed" "! command -v sudo"
	check "no setuid or setgid binaries" "[ -z \"\$(find / -xdev \\( -perm -4000 -o -perm -2000 \\) -type f 2>/dev/null)\" ]"
else
	check "agent has no sudo rights" "! sudo -l -U agent | grep -q '(.*)'"
	check "sudo is the only setuid binary" "[ \"\$(find / -xdev \\( -perm -4000 -o -perm -2000 \\) -type f 2>/dev/null)\" = /usr/bin/sudo ]"
fi
check "iptables and ip6tables run" "iptables --version && ip6tables --version"
check "ip runs" "ip -V"
check "git runs" "git --version"
check "tini is at /usr/bin/tini" "[ -x /usr/bin/tini ]"
check "claude runs" "$TOOLS/bin/claude --version"
check "codex runs" "$TOOLS/bin/codex --version"
check "bootstrap is root-owned and not writable by others" "[ \"\$(stat -c %U:%a $OF_DIR/bootstrap.sh)\" = root:755 ]"
[ $fail -eq 0 ] || die "self-check failed"
log "done"
