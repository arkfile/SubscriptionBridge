#!/bin/bash
# First-time VPS install and later binary updates for Subscription Bridge.
# TLS is Caddy + Let's Encrypt DNS-01 via deSEC. The bridge binds loopback HTTP.
#
# Usage:
#   sudo bash scripts/deploy.sh install --domain billing.example.com \
#     --consumer-url https://app.example.com/api/webhooks/subscription-bridge
#   sudo bash scripts/deploy.sh update
#
# Domain and consumer URL are required and have no product-specific defaults.
# The consumer webhook path is chosen by the consumer application.

set -euo pipefail

export PATH="/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.org}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Pinned toolchains. Go matches go.mod / CI. Caddy+deSEC match the Arkfile VPS pins.
# github.com/caddy-dns/desec tags start at v1.0.0 (v0.2.x is libdns/desec, not this module).
GO_VERSION="${GO_VERSION:-1.26.6}"
GO_SHA256_LINUX_AMD64="708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89"
GO_SHA256_LINUX_ARM64="d0507e9e9d7fe012aae570108cbd76c15de879e17130ab8cb90d4d7445cb1f2e"
XCADDY_VERSION="${XCADDY_VERSION:-v0.4.6}"
CADDY_VERSION="${CADDY_VERSION:-v2.11.4}"
CADDY_DESEC_MODULE="${CADDY_DESEC_MODULE:-github.com/caddy-dns/desec@v1.1.0}"
OS_FAMILY=""

BRIDGE_USER="bridge"
BRIDGE_GROUP="bridge"
BRIDGE_ENV="/etc/subscription-bridge/bridge.env"
BRIDGE_PLANS="/etc/subscription-bridge/plans.yaml"
BRIDGE_STATE="/var/lib/subscription-bridge"
CADDY_ENV="/var/lib/caddy/caddy-env"
BUILD_ROOT="${SUBSCRIPTION_BRIDGE_BUILD_DIR:-/var/tmp/subscription-bridge-build}"
BUILD_BIN="$BUILD_ROOT/bin"

COMMAND=""
DOMAIN="${DOMAIN:-}"
CONSUMER_URL="${CONSUMER_URL:-}"
ACME_EMAIL="${ACME_EMAIL:-}"
DESEC_TOKEN="${DESEC_TOKEN:-}"
MIGRATE=false
REBUILD_CADDY=false
EXTERNAL_FIREWALL_CONFIRMED=false
DESEC_TOKEN_FROM_CLI=false
GENERATED_PAIRING_ROOT=""

print_status() {
    local status="$1"
    local message="$2"
    case "$status" in
        INFO) echo -e "  ${BLUE}INFO:${NC} ${message}" ;;
        SUCCESS) echo -e "  ${GREEN}SUCCESS:${NC} ${message}" ;;
        WARNING) echo -e "  ${YELLOW}WARNING:${NC} ${message}" ;;
        ERROR) echo -e "  ${RED}ERROR:${NC} ${message}" ;;
    esac
}

die() {
    print_status ERROR "$1"
    exit 1
}

show_help() {
    cat <<EOF
Subscription Bridge VPS deploy

Usage:
  sudo bash scripts/deploy.sh install [OPTIONS]
  sudo bash scripts/deploy.sh update [OPTIONS]

install is the default command.

Required for a new install (prompted if omitted):
  --domain <hostname>         Public hostname of this bridge, e.g. billing.example.com
  --consumer-url <url>        Full HTTPS consumer callback URL (path is consumer-defined)

Optional:
  --acme-email <email>        Let's Encrypt notice address
  --migrate                   Run bridge-cli migrate after install/update
  --rebuild-caddy             Rebuild Caddy even if the pinned binary is already installed
  --external-firewall-confirmed
                              Continue when no local ufw/firewalld is present
  --desec-token <token>       Discouraged; prefer the interactive prompt
  -h, --help                  Show this help

The deSEC API token and database URL are prompted so they are not visible in
process listings. There is no default domain or consumer URL.

On Debian/Ubuntu and RHEL-family hosts the script installs curl, git, openssl,
the local firewall tool, and pinned Go ${GO_VERSION} from go.dev when needed.
It then builds Caddy ${CADDY_VERSION} with xcaddy ${XCADDY_VERSION} and
${CADDY_DESEC_MODULE}, the same pins used by the Arkfile VPS deploy path.
Distro golang packages are not used.

Examples:
  sudo bash scripts/deploy.sh install \\
    --domain billing.example.com \\
    --consumer-url https://app.example.com/api/webhooks/subscription-bridge \\
    --acme-email ops@example.com --migrate

  sudo bash scripts/deploy.sh update
EOF
}

# Print status lines for a command running as the pre-sudo user.
run_as_user() {
    if [ "$EUID" -eq 0 ] && [ -n "${SUDO_USER:-}" ]; then
        sudo -u "$SUDO_USER" -H "$@"
    else
        "$@"
    fi
}

find_go_binary() {
    local candidate
    for candidate in /usr/local/go/bin/go "$(command -v go 2>/dev/null || true)" /usr/local/bin/go /usr/bin/go; do
        if [ -n "$candidate" ] && [ -x "$candidate" ]; then
            echo "$candidate"
            return 0
        fi
    done
    return 1
}

detect_os_family() {
    local ids=""
    if [ -f /etc/debian_version ]; then
        echo debian
        return 0
    fi
    if [ -f /etc/redhat-release ]; then
        echo rhel
        return 0
    fi
    if [ -f /etc/os-release ]; then
        ids=$(. /etc/os-release; printf '%s %s' "${ID:-}" "${ID_LIKE:-}")
        case " $ids " in
            *debian*|*ubuntu*)
                echo debian
                return 0
                ;;
            *rhel*|*fedora*|*centos*|*almalinux*|*rocky*|*ol*)
                echo rhel
                return 0
                ;;
        esac
    fi
    echo unknown
}

go_version_string() {
    local bin="$1"
    if [ ! -x "$bin" ]; then
        return 1
    fi
    "$bin" version 2>/dev/null | awk '{
        if (match($0, /go[0-9]+\.[0-9]+(\.[0-9]+)?/)) {
            print substr($0, RSTART+2, RLENGTH-2)
            exit
        }
    }'
}

go_version_ge() {
    local have="$1"
    local need="$2"
    local lowest
    lowest=$(printf '%s\n%s\n' "$need" "$have" | sort -V | head -1)
    [ "$lowest" = "$need" ]
}

linux_go_arch() {
    case "$(uname -m)" in
        x86_64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        *)
            die "Unsupported architecture $(uname -m); install needs linux-amd64 or linux-arm64"
            ;;
    esac
}

install_host_packages() {
    OS_FAMILY=$(detect_os_family)
    print_status INFO "OS family: $OS_FAMILY"
    case "$OS_FAMILY" in
        debian)
            export DEBIAN_FRONTEND=noninteractive
            print_status INFO "Installing Debian/Ubuntu packages..."
            apt-get update -y
            apt-get install -y --no-install-recommends \
                ca-certificates curl git openssl tar xz-utils ufw libcap2-bin
            apt-get install -y --no-install-recommends dnsutils \
                || apt-get install -y --no-install-recommends bind9-dnsutils \
                || true
            print_status SUCCESS "Host packages installed"
            ;;
        rhel)
            local pkg
            if command -v dnf >/dev/null 2>&1; then
                pkg=dnf
            elif command -v yum >/dev/null 2>&1; then
                pkg=yum
            else
                die "dnf/yum not found on RHEL-family host"
            fi
            print_status INFO "Installing RHEL-family packages..."
            "$pkg" install -y \
                ca-certificates curl git openssl tar gzip firewalld libcap bind-utils
            systemctl enable --now firewalld >/dev/null 2>&1 || true
            print_status SUCCESS "Host packages installed"
            ;;
        *)
            die "Unsupported OS. scripts/deploy.sh supports Debian/Ubuntu and RHEL-family (RHEL, Alma, Rocky, Fedora)."
            ;;
    esac
}

install_official_go() {
    local arch filename url sha tmp expected
    arch=$(linux_go_arch)
    case "$arch" in
        amd64) sha="$GO_SHA256_LINUX_AMD64" ;;
        arm64) sha="$GO_SHA256_LINUX_ARM64" ;;
    esac
    filename="go${GO_VERSION}.linux-${arch}.tar.gz"
    url="https://go.dev/dl/${filename}"
    tmp=$(mktemp -d)
    print_status INFO "Downloading Go ${GO_VERSION} (${arch}) from go.dev..."
    if ! curl -fsSL "$url" -o "$tmp/$filename"; then
        rm -rf "$tmp"
        die "Failed to download $url"
    fi
    expected="${sha}  ${tmp}/${filename}"
    if ! printf '%s\n' "$expected" | sha256sum -c -; then
        rm -rf "$tmp"
        die "Go tarball SHA-256 mismatch"
    fi
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tmp/$filename"
    rm -rf "$tmp"
    if [ ! -x /usr/local/go/bin/go ]; then
        die "Go extract did not produce /usr/local/go/bin/go"
    fi
    print_status SUCCESS "Installed Go ${GO_VERSION} to /usr/local/go"
}

ensure_go() {
    local have="" bin=""
    export PATH="/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    if bin=$(find_go_binary); then
        have=$(go_version_string "$bin" || true)
        if [ -n "$have" ] && go_version_ge "$have" "$GO_VERSION"; then
            GO_BINARY="$bin"
            export GO_BINARY
            print_status SUCCESS "Using Go $have at $GO_BINARY"
            return 0
        fi
        if [ -n "$have" ]; then
            print_status INFO "Go $have at $bin is older than ${GO_VERSION}; installing official toolchain"
        fi
    fi
    install_official_go
    GO_BINARY="/usr/local/go/bin/go"
    export GO_BINARY
    have=$(go_version_string "$GO_BINARY")
    if [ -z "$have" ] || ! go_version_ge "$have" "$GO_VERSION"; then
        die "Installed Go does not meet ${GO_VERSION}"
    fi
    print_status SUCCESS "Using Go $have at $GO_BINARY"
}

is_tty() {
    [ -t 0 ]
}

prompt_nonempty() {
    local prompt_text="$1"
    local value=""
    if ! is_tty; then
        die "missing required value in non-interactive mode: $prompt_text"
    fi
    while true; do
        read -r -p "$prompt_text" value
        if [ -n "$value" ]; then
            printf '%s' "$value"
            return 0
        fi
        print_status WARNING "A value is required" >&2
    done
}

prompt_optional() {
    local prompt_text="$1"
    local value=""
    if ! is_tty; then
        printf '%s' ""
        return 0
    fi
    read -r -p "$prompt_text" value
    printf '%s' "$value"
}

prompt_secret() {
    local prompt_text="$1"
    local allow_empty="$2"
    local value=""
    if ! is_tty; then
        if [ "$allow_empty" = "true" ]; then
            printf '%s' ""
            return 0
        fi
        die "missing required secret in non-interactive mode"
    fi
    while true; do
        read -r -s -p "$prompt_text" value
        echo >&2
        if [ -n "$value" ] || [ "$allow_empty" = "true" ]; then
            printf '%s' "$value"
            return 0
        fi
        print_status WARNING "A value is required" >&2
    done
}

# Read KEY=value from an env file without shell-expanding the value.
env_get() {
    local key="$1"
    local file="$2"
    local line=""
    line=$(grep -E "^${key}=" "$file" 2>/dev/null | tail -n1 || true)
    if [ -z "$line" ]; then
        printf '%s' ""
        return 0
    fi
    printf '%s' "${line#*=}"
}

# Export known bridge keys from the env file without expanding '$' in values.
load_bridge_env() {
    local file="$1"
    local line key value
    while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in
            ''|'#'*) continue ;;
        esac
        key="${line%%=*}"
        value="${line#*=}"
        case "$key" in
            BRIDGE_*|CONSUMER_WEBHOOK_URL|STRIPE_*|ADYEN_*)
                printf -v "$key" '%s' "$value"
                export "$key"
                ;;
        esac
    done < "$file"
}

host_from_public_url() {
    local raw="$1"
    raw="${raw#http://}"
    raw="${raw#https://}"
    raw="${raw%%/*}"
    printf '%s' "$raw"
}

validate_domain() {
    local host="$1"
    case "$host" in
        ''|*/*|*:*|*\\*|*' '*|*@*)
            return 1
            ;;
    esac
    if [[ "$host" != *.* ]]; then
        return 1
    fi
    printf '%s' "${host,,}"
}

validate_https_url() {
    local raw="$1"
    case "$raw" in
        https://*)
            ;;
        *)
            return 1
            ;;
    esac
    case "$raw" in
        *' '*|*@*|*\\*)
            return 1
            ;;
    esac
    printf '%s' "$raw"
}

detect_public_ip() {
    local public_ip=""
    public_ip=$(curl -fsS https://api.ipify.org 2>/dev/null || true)
    if [ -z "$public_ip" ]; then
        public_ip=$(curl -fsS https://ifconfig.me 2>/dev/null || true)
    fi
    printf '%s' "$public_ip"
}

resolve_domain_ip() {
    local domain="$1"
    local resolved_ip=""
    if command -v dig >/dev/null 2>&1; then
        resolved_ip=$(dig +short A "$domain" | head -1)
    else
        resolved_ip=$(getent ahostsv4 "$domain" 2>/dev/null | awk '{print $1}' | head -1)
    fi
    printf '%s' "$resolved_ip"
}

configure_firewall() {
    local os_family="${1:-$OS_FAMILY}"
    print_status INFO "Configuring firewall..."
    if [ "$os_family" = "rhel" ]; then
        if command -v firewall-cmd >/dev/null 2>&1; then
            systemctl enable --now firewalld >/dev/null 2>&1 || true
            firewall-cmd --set-default-zone=drop >/dev/null
            firewall-cmd --permanent --add-service=ssh >/dev/null
            firewall-cmd --permanent --add-service=http >/dev/null
            firewall-cmd --permanent --add-service=https >/dev/null
            firewall-cmd --reload >/dev/null
            print_status SUCCESS "firewalld configured for ssh/http/https only"
            return 0
        fi
    fi
    if [ "$os_family" = "debian" ] && command -v ufw >/dev/null 2>&1; then
        ufw default deny incoming >/dev/null
        ufw default allow outgoing >/dev/null
        ufw allow 22/tcp >/dev/null
        ufw allow 80/tcp >/dev/null
        ufw allow 443/tcp >/dev/null
        ufw --force enable >/dev/null
        print_status SUCCESS "ufw configured for ssh/http/https only"
        return 0
    fi
    if [ "$EXTERNAL_FIREWALL_CONFIRMED" = true ]; then
        print_status WARNING "No local firewall detected; --external-firewall-confirmed is set"
        return 0
    fi
    die "No supported firewall (firewalld/ufw). Re-run with --external-firewall-confirmed if an external firewall is in place."
}

ensure_users_and_dirs() {
    if ! getent group "$BRIDGE_GROUP" >/dev/null; then
        groupadd -r "$BRIDGE_GROUP"
    fi
    if ! getent passwd "$BRIDGE_USER" >/dev/null; then
        useradd -r -g "$BRIDGE_GROUP" -d "$BRIDGE_STATE" -s /usr/sbin/nologin -c "Subscription Bridge" "$BRIDGE_USER"
    fi
    if ! getent group caddy >/dev/null; then
        groupadd -r caddy
    fi
    if ! getent passwd caddy >/dev/null; then
        useradd -r -g caddy -d /var/lib/caddy -s /usr/sbin/nologin -c "Caddy Service Account" caddy
    fi
    install -d -m 750 -o root -g "$BRIDGE_GROUP" /etc/subscription-bridge
    install -d -m 700 -o "$BRIDGE_USER" -g "$BRIDGE_GROUP" "$BRIDGE_STATE"
    install -d -m 755 -o caddy -g caddy /var/lib/caddy
    install -d -m 755 -o caddy -g caddy /var/log/caddy
    install -d -m 755 -o root -g root /etc/caddy
}

verify_caddy_binary() {
    local caddy_binary="$1"
    local expected_version="${CADDY_VERSION#v}"
    local desec_package="${CADDY_DESEC_MODULE%@*}"
    local desec_version="${CADDY_DESEC_MODULE##*@}"
    local actual_version
    if [ ! -x "$caddy_binary" ]; then
        print_status ERROR "Caddy candidate is not executable: $caddy_binary"
        return 1
    fi
    actual_version=$("$caddy_binary" version 2>/dev/null | awk '{print $1}')
    if [ "$actual_version" != "v${expected_version}" ]; then
        print_status ERROR "Caddy candidate version is $actual_version; expected v${expected_version}"
        return 1
    fi
    if ! "$caddy_binary" list-modules 2>/dev/null | grep -qx "dns.providers.desec"; then
        print_status ERROR "Caddy candidate does not contain dns.providers.desec"
        return 1
    fi
    if ! "$GO_BINARY" version -m "$caddy_binary" 2>/dev/null |
        awk -v package="$desec_package" -v version="$desec_version" '
            $1 == "dep" && $2 == package && $3 == version { found = 1 }
            END { exit(found ? 0 : 1) }
        '; then
        print_status ERROR "Caddy candidate does not contain pinned ${CADDY_DESEC_MODULE}"
        return 1
    fi
    print_status SUCCESS "Verified Caddy $actual_version with dns.providers.desec"
    return 0
}

build_caddy_binary() {
    local output_path="$1"
    local gopath xcaddy_bin go_bin_dir caddy_build_path
    mkdir -p "$(dirname "$output_path")"
    if [ "$EUID" -eq 0 ] && [ -n "${SUDO_USER:-}" ]; then
        chown -R "$SUDO_USER:$SUDO_USER" "$BUILD_ROOT"
    fi
    print_status INFO "Installing pinned xcaddy ${XCADDY_VERSION}..."
    if ! run_as_user env \
        "PATH=$(dirname "$GO_BINARY"):/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
        GOPROXY="$GOPROXY" \
        GOSUMDB="$GOSUMDB" \
        CGO_ENABLED=0 \
        "$GO_BINARY" install "github.com/caddyserver/xcaddy/cmd/xcaddy@${XCADDY_VERSION}"; then
        die "Failed to install pinned xcaddy"
    fi
    gopath=$(run_as_user "$GO_BINARY" env GOPATH 2>/dev/null | tr -d '\r')
    xcaddy_bin="${gopath}/bin/xcaddy"
    if [ -z "$gopath" ] || [ ! -x "$xcaddy_bin" ]; then
        die "xcaddy binary not found after installation"
    fi
    rm -f "$output_path"
    go_bin_dir="$(dirname "$GO_BINARY")"
    # xcaddy launches `go` by name. sudo -u sanitizes PATH, so keep the Go directory first.
    caddy_build_path="${go_bin_dir}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    print_status INFO "Building Caddy ${CADDY_VERSION} with ${CADDY_DESEC_MODULE}..."
    if ! run_as_user env \
        "PATH=$caddy_build_path" \
        GOPROXY="$GOPROXY" \
        GOSUMDB="$GOSUMDB" \
        CGO_ENABLED=0 \
        "$xcaddy_bin" build "$CADDY_VERSION" \
        --with "$CADDY_DESEC_MODULE" \
        --output "$output_path"; then
        die "Failed to build pinned Caddy with the deSEC module"
    fi
    chmod 755 "$output_path"
    if ! verify_caddy_binary "$output_path"; then
        die "Built Caddy failed pin verification"
    fi
}

install_caddy_binary() {
    mkdir -p "$BUILD_BIN"
    if [ "$REBUILD_CADDY" != true ] && [ -x /usr/local/bin/caddy ] && verify_caddy_binary /usr/local/bin/caddy; then
        print_status SUCCESS "Pinned Caddy already installed"
        return 0
    fi
    build_caddy_binary "$BUILD_BIN/caddy"
    install -m 755 -o root -g root "$BUILD_BIN/caddy" /usr/local/bin/caddy
    if command -v setcap >/dev/null 2>&1; then
        setcap cap_net_bind_service=+ep /usr/local/bin/caddy
    fi
    if ! verify_caddy_binary /usr/local/bin/caddy; then
        die "Installed Caddy failed pin verification"
    fi
    print_status SUCCESS "Caddy deployed to /usr/local/bin/caddy"
}

render_caddyfile() {
    local tmp_global
    tmp_global=$(mktemp)
    if [ -n "$ACME_EMAIL" ]; then
        cat > "$tmp_global" <<EOF
{
	email ${ACME_EMAIL}

	servers {
		protocols h1 h2 h3
		strict_sni_host
	}
}
EOF
    else
        cat > "$tmp_global" <<EOF
{
	servers {
		protocols h1 h2 h3
		strict_sni_host
	}
}
EOF
    fi
    awk -v gfile="$tmp_global" '
        /\{GLOBAL_BLOCK\}/ {
            while ((getline line < gfile) > 0) print line
            close(gfile)
            next
        }
        { print }
    ' "$REPO_ROOT/deploy/Caddyfile.prod" | sed "s|{DOMAIN}|${DOMAIN}|g" > /etc/caddy/Caddyfile
    rm -f "$tmp_global"
    chmod 644 /etc/caddy/Caddyfile
}

write_caddy_env() {
    local old_umask
    old_umask=$(umask)
    umask 077
    printf 'DESEC_TOKEN=%s\n' "$DESEC_TOKEN" > "$CADDY_ENV"
    umask "$old_umask"
    chown caddy:caddy "$CADDY_ENV"
    chmod 600 "$CADDY_ENV"
}

install_units() {
    install -m 644 -o root -g root "$REPO_ROOT/deploy/subscription-bridge.service" /etc/systemd/system/subscription-bridge.service
    install -m 644 -o root -g root "$REPO_ROOT/deploy/caddy.service" /etc/systemd/system/caddy.service
    mkdir -p /etc/systemd/system/caddy.service.d
    cat > /etc/systemd/system/caddy.service.d/env.conf <<EOF
[Service]
EnvironmentFile=/var/lib/caddy/caddy-env
EOF
    systemctl daemon-reload
}

maybe_selinux() {
    if ! command -v getenforce >/dev/null 2>&1; then
        return 0
    fi
    local status
    status=$(getenforce 2>/dev/null || echo Disabled)
    if [ "$status" != Enforcing ]; then
        return 0
    fi
    print_status INFO "Applying SELinux booleans for Caddy"
    setsebool -P httpd_can_network_connect 1 || true
}

build_bridge_binaries() {
    local go_bin_dir
    print_status INFO "Building bridge and bridge-cli..."
    mkdir -p "$BUILD_BIN"
    if [ "$EUID" -eq 0 ] && [ -n "${SUDO_USER:-}" ]; then
        chown -R "$SUDO_USER:$SUDO_USER" "$BUILD_ROOT"
    fi
    go_bin_dir="$(dirname "$GO_BINARY")"
    if ! run_as_user env \
        "PATH=${go_bin_dir}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
        GOPROXY="$GOPROXY" \
        GOSUMDB="$GOSUMDB" \
        CGO_ENABLED=0 \
        GOFLAGS="${GOFLAGS:-}" \
        "$GO_BINARY" build -trimpath -o "$BUILD_BIN/bridge" ./cmd/bridge; then
        die "Failed to build ./cmd/bridge"
    fi
    if ! run_as_user env \
        "PATH=${go_bin_dir}:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
        GOPROXY="$GOPROXY" \
        GOSUMDB="$GOSUMDB" \
        CGO_ENABLED=0 \
        "$GO_BINARY" build -trimpath -o "$BUILD_BIN/bridge-cli" ./cmd/bridge-cli; then
        die "Failed to build ./cmd/bridge-cli"
    fi
    install -m 755 -o root -g root "$BUILD_BIN/bridge" /usr/local/bin/bridge
    install -m 755 -o root -g root "$BUILD_BIN/bridge-cli" /usr/local/bin/bridge-cli
    print_status SUCCESS "Installed /usr/local/bin/bridge and /usr/local/bin/bridge-cli"
}

write_bridge_env_if_missing() {
    if [ -f "$BRIDGE_ENV" ]; then
        print_status INFO "Keeping existing $BRIDGE_ENV"
        return 0
    fi
    local pairing_root database_url stripe_key stripe_wh adyen_key adyen_hmac adyen_client adyen_enc adyen_env adyen_prefix
    pairing_root=$(prompt_secret "Pairing root (blank to generate): " true)
    if [ -z "$pairing_root" ]; then
        pairing_root=$(openssl rand -hex 32)
        GENERATED_PAIRING_ROOT="$pairing_root"
        print_status SUCCESS "Generated BRIDGE_CONSUMER_PAIRING_ROOT"
    else
        if ! [[ "$pairing_root" =~ ^[0-9a-f]{64}$ ]]; then
            die "Pairing root must be exactly 64 lowercase hexadecimal characters"
        fi
    fi
    database_url=$(prompt_secret "BRIDGE_DATABASE_URL: " false)
    if is_tty; then
        echo
        echo "Processor credentials may be left blank and filled in $BRIDGE_ENV later."
        echo "Startup fails closed until every provider selected by plans.yaml has credentials."
        echo
    fi
    stripe_key=$(prompt_secret "STRIPE_SECRET_KEY (blank to skip): " true)
    stripe_wh=$(prompt_secret "STRIPE_WEBHOOK_SECRET (blank to skip): " true)
    adyen_key=$(prompt_secret "ADYEN_API_KEY (blank to skip): " true)
    adyen_hmac=$(prompt_secret "ADYEN_HMAC_KEY (blank to skip): " true)
    adyen_client=$(prompt_secret "ADYEN_CLIENT_KEY (blank to skip): " true)
    adyen_enc=$(prompt_secret "ADYEN_DATA_ENCRYPTION_KEY (blank to skip): " true)
    adyen_env=$(prompt_optional "ADYEN_ENVIRONMENT [test]: ")
    if [ -z "$adyen_env" ]; then
        adyen_env=test
    fi
    adyen_prefix=""
    if [ "$adyen_env" = live ]; then
        adyen_prefix=$(prompt_optional "ADYEN_LIVE_PREFIX: ")
    fi
    local old_umask
    old_umask=$(umask)
    umask 077
    {
        printf 'BRIDGE_PUBLIC_URL=https://%s\n' "$DOMAIN"
        printf 'CONSUMER_WEBHOOK_URL=%s\n' "$CONSUMER_URL"
        printf 'BRIDGE_CONSUMER_PAIRING_ROOT=%s\n' "$pairing_root"
        printf 'BRIDGE_LISTEN=127.0.0.1:8081\n'
        printf 'BRIDGE_PLANS_PATH=%s\n' "$BRIDGE_PLANS"
        printf 'BRIDGE_DATABASE_URL=%s\n' "$database_url"
        printf 'BRIDGE_DEFAULT_PROCESSOR=stripe\n'
        printf 'STRIPE_SECRET_KEY=%s\n' "$stripe_key"
        printf 'STRIPE_WEBHOOK_SECRET=%s\n' "$stripe_wh"
        printf 'ADYEN_API_KEY=%s\n' "$adyen_key"
        printf 'ADYEN_HMAC_KEY=%s\n' "$adyen_hmac"
        printf 'ADYEN_CLIENT_KEY=%s\n' "$adyen_client"
        printf 'ADYEN_LIVE_PREFIX=%s\n' "$adyen_prefix"
        printf 'ADYEN_ENVIRONMENT=%s\n' "$adyen_env"
        printf 'ADYEN_DATA_ENCRYPTION_KEY=%s\n' "$adyen_enc"
        printf 'BRIDGE_LOG_LEVEL=info\n'
        printf 'BRIDGE_SCHEDULER_ENABLED=true\n'
        printf 'BRIDGE_RENEWAL_RETRY_DELAYS=24h,72h,120h\n'
        printf 'BRIDGE_DUNNING_TERMINATION_DELAY=0s\n'
        printf 'BRIDGE_ADYEN_RESOLUTION_DEADLINE=144h\n'
        printf 'BRIDGE_PROVIDER_PAYLOAD_QUARANTINE_ENABLED=false\n'
        printf 'BRIDGE_PROVIDER_PAYLOAD_QUARANTINE_MAX_RETENTION=168h\n'
    } > "$BRIDGE_ENV"
    umask "$old_umask"
    chown root:"$BRIDGE_GROUP" "$BRIDGE_ENV"
    chmod 640 "$BRIDGE_ENV"
    print_status SUCCESS "Wrote $BRIDGE_ENV"
}

install_plans_if_missing() {
    if [ -f "$BRIDGE_PLANS" ]; then
        print_status INFO "Keeping existing $BRIDGE_PLANS"
        return 0
    fi
    install -m 640 -o root -g "$BRIDGE_GROUP" "$REPO_ROOT/config/plans.example.yaml" "$BRIDGE_PLANS"
    print_status WARNING "Installed example $BRIDGE_PLANS — replace placeholder SKUs before going live"
}

run_bridge_cli() {
    load_bridge_env "$BRIDGE_ENV"
    /usr/local/bin/bridge-cli "$@"
}

maybe_migrate() {
    if [ "$MIGRATE" != true ]; then
        if is_tty; then
            local answer
            read -r -p "Run database migrations now? [y/N]: " answer
            case "$answer" in
                y|Y|yes|YES) MIGRATE=true ;;
            esac
        fi
    fi
    if [ "$MIGRATE" != true ]; then
        print_status INFO "Skipping migrations (pass --migrate later)"
        return 0
    fi
    print_status INFO "Applying schema migrations..."
    if ! run_bridge_cli migrate; then
        die "bridge-cli migrate failed"
    fi
    print_status SUCCESS "Schema migrations applied"
}

validate_caddyfile() {
    if ! DESEC_TOKEN="$DESEC_TOKEN" /usr/local/bin/caddy validate --config /etc/caddy/Caddyfile; then
        die "Caddyfile validation failed"
    fi
    print_status SUCCESS "Caddyfile validated"
}

start_caddy() {
    systemctl enable caddy
    systemctl restart caddy
    local i code="000"
    print_status INFO "Waiting for public HTTPS (DNS-01 may take up to a minute)..."
    for i in $(seq 1 30); do
        code=$(curl -sk -o /dev/null -w "%{http_code}" "https://${DOMAIN}/health" || true)
        if [ "$code" != "000" ]; then
            print_status SUCCESS "Caddy is serving https://${DOMAIN} (HTTP ${code})"
            return 0
        fi
        sleep 3
    done
    die "Timed out waiting for https://${DOMAIN}. Check: journalctl -u caddy -f"
}

try_start_bridge() {
    print_status INFO "Checking configuration..."
    if ! run_bridge_cli check-config; then
        print_status WARNING "Configuration is not ready; Caddy is up, bridge is not started"
        print_status WARNING "Edit $BRIDGE_ENV and $BRIDGE_PLANS, then: systemctl start subscription-bridge"
        return 0
    fi
    systemctl enable subscription-bridge
    systemctl restart subscription-bridge
    local i
    for i in $(seq 1 15); do
        if curl -sk "https://${DOMAIN}/health" 2>/dev/null | grep -qx "ok"; then
            print_status SUCCESS "Bridge is healthy at https://${DOMAIN}/health"
            return 0
        fi
        sleep 2
    done
    print_status WARNING "Bridge unit started but /health did not return ok"
    print_status WARNING "Check: journalctl -u subscription-bridge -f"
}

print_next_steps() {
    echo
    echo -e "${GREEN}DEPLOY COMPLETE${NC}"
    echo
    echo "  Public origin:     https://${DOMAIN}"
    echo "  Consumer callback: ${CONSUMER_URL}"
    echo "  Stripe webhook:    https://${DOMAIN}/v1/webhooks/stripe"
    echo "  Adyen webhook:     https://${DOMAIN}/v1/webhooks/adyen"
    echo
    echo "Configure the consumer with this bridge origin, the same pairing root,"
    echo "and the callback URL above. The pairing root is in $BRIDGE_ENV."
    if [ -n "$GENERATED_PAIRING_ROOT" ]; then
        echo
        echo -e "${YELLOW}Generated pairing root (copy to the consumer now):${NC}"
        echo "  $GENERATED_PAIRING_ROOT"
        echo
    fi
    echo "Useful commands:"
    echo "  sudo journalctl -u subscription-bridge -f"
    echo "  sudo journalctl -u caddy -f"
    echo "  sudo bash scripts/deploy.sh update"
    echo
}

require_root() {
    if [ "$EUID" -ne 0 ]; then
        die "This script must be run with sudo"
    fi
}

require_repo() {
    cd "$REPO_ROOT"
    if [ ! -f go.mod ] || [ ! -f "$REPO_ROOT/deploy/Caddyfile.prod" ]; then
        die "Run this script from a Subscription Bridge checkout (missing go.mod or deploy/Caddyfile.prod)"
    fi
}

parse_args() {
    if [ $# -gt 0 ]; then
        case "$1" in
            install|update)
                COMMAND="$1"
                shift
                ;;
            -h|--help)
                show_help
                exit 0
                ;;
            *)
                COMMAND="install"
                ;;
        esac
    fi
    COMMAND="${COMMAND:-install}"
    while [ $# -gt 0 ]; do
        case "$1" in
            --domain)
                DOMAIN="$2"
                shift 2
                ;;
            --consumer-url)
                CONSUMER_URL="$2"
                shift 2
                ;;
            --acme-email)
                ACME_EMAIL="$2"
                shift 2
                ;;
            --desec-token)
                DESEC_TOKEN="$2"
                DESEC_TOKEN_FROM_CLI=true
                shift 2
                ;;
            --migrate)
                MIGRATE=true
                shift
                ;;
            --rebuild-caddy)
                REBUILD_CADDY=true
                shift
                ;;
            --external-firewall-confirmed)
                EXTERNAL_FIREWALL_CONFIRMED=true
                shift
                ;;
            -h|--help)
                show_help
                exit 0
                ;;
            *)
                echo "Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
    done
}

resolve_identity_from_env() {
    if [ ! -f "$BRIDGE_ENV" ]; then
        return 0
    fi
    local existing_url existing_consumer
    existing_url=$(env_get BRIDGE_PUBLIC_URL "$BRIDGE_ENV")
    existing_consumer=$(env_get CONSUMER_WEBHOOK_URL "$BRIDGE_ENV")
    if [ -z "$DOMAIN" ] && [ -n "$existing_url" ]; then
        DOMAIN=$(host_from_public_url "$existing_url")
    fi
    if [ -z "$CONSUMER_URL" ] && [ -n "$existing_consumer" ]; then
        CONSUMER_URL="$existing_consumer"
    fi
    if [ -n "$DOMAIN" ] && [ -n "$existing_url" ]; then
        local existing_host
        existing_host=$(host_from_public_url "$existing_url")
        if [ "$DOMAIN" != "$existing_host" ]; then
            die "--domain $DOMAIN does not match BRIDGE_PUBLIC_URL host $existing_host"
        fi
    fi
}

ensure_identity() {
    if [ -z "$DOMAIN" ]; then
        DOMAIN=$(prompt_nonempty "Public hostname (e.g. billing.example.com): ")
    fi
    DOMAIN=$(validate_domain "$DOMAIN") || die "Invalid --domain (hostname only, e.g. billing.example.com)"
    if [ -z "$CONSUMER_URL" ]; then
        CONSUMER_URL=$(prompt_nonempty "Full consumer callback URL (https://...): ")
    fi
    CONSUMER_URL=$(validate_https_url "$CONSUMER_URL") || die "Invalid --consumer-url (must be an https URL)"
}

ensure_desec_token() {
    if [ -z "$DESEC_TOKEN" ] && [ -f "$CADDY_ENV" ]; then
        DESEC_TOKEN=$(env_get DESEC_TOKEN "$CADDY_ENV")
    fi
    if [ -z "$DESEC_TOKEN" ]; then
        print_status INFO "deSEC token is not taken from flags by default (process listings are visible)"
        DESEC_TOKEN=$(prompt_secret "Enter deSEC API token: " false)
    elif [ "$DESEC_TOKEN_FROM_CLI" = true ]; then
        print_status WARNING "deSEC token was passed via CLI and is visible in process listings"
        print_status WARNING "Omit --desec-token and enter it interactively instead"
    fi
}

preflight() {
    print_status INFO "Repository: $REPO_ROOT"
    install_host_packages
    ensure_go
    local cmd
    for cmd in git openssl curl tar sha256sum; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            die "Required tool still missing after package install: $cmd"
        fi
    done
    local public_ip domain_ip
    public_ip=$(detect_public_ip)
    [ -n "$public_ip" ] || die "Failed to detect this host's public IPv4"
    print_status INFO "Public IPv4: $public_ip"
    domain_ip=$(resolve_domain_ip "$DOMAIN")
    [ -n "$domain_ip" ] || die "Failed to resolve A record for $DOMAIN"
    print_status INFO "Resolved $DOMAIN to $domain_ip"
    if [ "$public_ip" != "$domain_ip" ]; then
        die "DNS mismatch: $DOMAIN resolves to $domain_ip but this host is $public_ip"
    fi
    print_status SUCCESS "DNS A record matches this host"
}

confirm_install() {
    echo
    echo -e "${CYAN}Subscription Bridge deploy${NC}"
    echo "  Command:       $COMMAND"
    echo "  Domain:        $DOMAIN"
    echo "  Consumer URL:  $CONSUMER_URL"
    if [ -n "$ACME_EMAIL" ]; then
        echo "  ACME email:    $ACME_EMAIL"
    else
        echo "  ACME email:    (not set)"
    fi
    echo "  This will install host packages and Go ${GO_VERSION} if they are missing,"
    echo "  then build Caddy ${CADDY_VERSION} with ${CADDY_DESEC_MODULE}."
    echo
    if ! is_tty; then
        return 0
    fi
    local answer
    read -r -p "Type DEPLOY to proceed (anything else cancels): " answer
    if [ "$answer" != DEPLOY ]; then
        echo "Cancelled. Nothing was changed."
        exit 0
    fi
}

cmd_install() {
    resolve_identity_from_env
    ensure_identity
    ensure_desec_token
    confirm_install
    preflight
    configure_firewall "$OS_FAMILY"
    ensure_users_and_dirs
    build_bridge_binaries
    install_caddy_binary
    write_caddy_env
    render_caddyfile
    install_units
    maybe_selinux
    validate_caddyfile
    write_bridge_env_if_missing
    install_plans_if_missing
    maybe_migrate
    start_caddy
    try_start_bridge
    print_next_steps
}

cmd_update() {
    [ -f "$BRIDGE_ENV" ] || die "No existing install ($BRIDGE_ENV missing). Run install first."
    resolve_identity_from_env
    ensure_identity
    if [ -z "$DESEC_TOKEN" ] && [ -f "$CADDY_ENV" ]; then
        DESEC_TOKEN=$(env_get DESEC_TOKEN "$CADDY_ENV")
    fi
    [ -n "$DESEC_TOKEN" ] || die "Missing deSEC token in $CADDY_ENV"
    if [ -z "$ACME_EMAIL" ] && [ -f /etc/caddy/Caddyfile ]; then
        ACME_EMAIL=$(awk '/^[[:space:]]*email / { print $2; exit }' /etc/caddy/Caddyfile || true)
    fi
    confirm_install
    preflight
    ensure_users_and_dirs
    build_bridge_binaries
    install_caddy_binary
    render_caddyfile
    install_units
    validate_caddyfile
    maybe_migrate
    start_caddy
    try_start_bridge
    print_next_steps
}

parse_args "$@"
require_root
require_repo

case "$COMMAND" in
    install) cmd_install ;;
    update) cmd_update ;;
    *)
        show_help
        exit 1
        ;;
esac
