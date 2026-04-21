#!/bin/bash
# LLMKube Installation Script
# Usage: curl -sSL https://raw.githubusercontent.com/defilantech/LLMKube/main/install.sh | bash
#
# Options (via environment variables):
#   LLMKUBE_VERSION        - Install specific version (default: latest)
#   LLMKUBE_INSTALL_DIR    - Installation directory (default: /usr/local/bin)
#   LLMKUBE_NO_SUDO        - Set to 1 to skip sudo (for user-local installs)
#   LLMKUBE_SKIP_CHECKSUM  - Set to 1 to skip sha256 verification (NOT recommended)

set -e

REPO="defilantech/LLMKube"
BINARY_NAME="llmkube"         # binary installed into $INSTALL_DIR
ARCHIVE_PREFIX="LLMKube"      # tarball filename prefix emitted by goreleaser
INSTALL_DIR="${LLMKUBE_INSTALL_DIR:-/usr/local/bin}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

# Detect OS
detect_os() {
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$OS" in
        linux*)  OS="linux" ;;
        darwin*) OS="darwin" ;;
        mingw*|msys*|cygwin*) OS="windows" ;;
        *) error "Unsupported operating system: $OS" ;;
    esac
    echo "$OS"
}

# Detect architecture
detect_arch() {
    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        arm64|aarch64) ARCH="arm64" ;;
        *) error "Unsupported architecture: $ARCH" ;;
    esac
    echo "$ARCH"
}

# Get latest version from GitHub API
get_latest_version() {
    if command -v curl &> /dev/null; then
        curl -sSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'
    elif command -v wget &> /dev/null; then
        wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'
    else
        error "curl or wget is required to download llmkube"
    fi
}

# Compute sha256 of a file, using whichever tool is available on this host.
compute_sha256() {
    local file="$1"
    if command -v sha256sum &> /dev/null; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum &> /dev/null; then
        shasum -a 256 "$file" | awk '{print $1}'
    else
        error "Neither sha256sum nor shasum is available; cannot verify integrity. Install one, or set LLMKUBE_SKIP_CHECKSUM=1 to bypass (NOT recommended)."
    fi
}

# Fetch the release checksums.txt and verify the downloaded archive matches.
# Aborts on any failure unless LLMKUBE_SKIP_CHECKSUM=1 is set.
verify_checksum() {
    local archive="$1"
    local filename="$2"
    local checksums_url="$3"
    local tmp_dir="$4"

    if [[ "${LLMKUBE_SKIP_CHECKSUM:-0}" == "1" ]]; then
        warn "LLMKUBE_SKIP_CHECKSUM=1 — skipping integrity verification. Not recommended for production."
        return 0
    fi

    info "Fetching checksums.txt..."
    if command -v curl &> /dev/null; then
        curl -fsSL "$checksums_url" -o "$tmp_dir/checksums.txt" \
            || error "Failed to fetch checksums.txt from $checksums_url. Set LLMKUBE_SKIP_CHECKSUM=1 to bypass (NOT recommended)."
    else
        wget -q "$checksums_url" -O "$tmp_dir/checksums.txt" \
            || error "Failed to fetch checksums.txt from $checksums_url. Set LLMKUBE_SKIP_CHECKSUM=1 to bypass (NOT recommended)."
    fi

    local expected
    expected=$(awk -v fname="$filename" '$2 == fname { print $1 }' "$tmp_dir/checksums.txt")
    if [[ -z "$expected" ]]; then
        error "No checksum entry for $filename in checksums.txt. Set LLMKUBE_SKIP_CHECKSUM=1 to bypass (NOT recommended)."
    fi

    local actual
    actual=$(compute_sha256 "$archive")
    if [[ "$actual" != "$expected" ]]; then
        error "Checksum mismatch for $filename
  expected: $expected
  actual:   $actual
Refusing to install a binary that does not match its published checksum."
    fi

    info "Checksum verified (sha256: ${actual:0:12}…)"
}

# Download and install
download_and_install() {
    local version="$1"
    local os="$2"
    local arch="$3"

    # Remove 'v' prefix for filename
    local version_num="${version#v}"

    # Construct download URL (archive name follows goreleaser's "{{ .ProjectName }}_..."
    # template, which resolves to "LLMKube_..." for this project.)
    local filename="${ARCHIVE_PREFIX}_${version_num}_${os}_${arch}.tar.gz"
    local url="https://github.com/${REPO}/releases/download/${version}/${filename}"
    local checksums_url="https://github.com/${REPO}/releases/download/${version}/checksums.txt"

    info "Downloading llmkube ${version} for ${os}/${arch}..."

    # Create temp directory
    local tmp_dir=$(mktemp -d)
    trap "rm -rf $tmp_dir" EXIT

    # Download (use -f so HTTP 4xx/5xx fail fast instead of silently saving an error page)
    if command -v curl &> /dev/null; then
        curl -fsSL "$url" -o "$tmp_dir/llmkube.tar.gz" || error "Failed to download from $url"
    else
        wget -q "$url" -O "$tmp_dir/llmkube.tar.gz" || error "Failed to download from $url"
    fi

    # Verify checksum before unpacking any untrusted bytes
    verify_checksum "$tmp_dir/llmkube.tar.gz" "$filename" "$checksums_url" "$tmp_dir"

    # Extract
    info "Extracting..."
    tar -xzf "$tmp_dir/llmkube.tar.gz" -C "$tmp_dir"

    # Install
    info "Installing to ${INSTALL_DIR}..."
    if [[ -w "$INSTALL_DIR" ]] || [[ "${LLMKUBE_NO_SUDO:-0}" == "1" ]]; then
        mv "$tmp_dir/$BINARY_NAME" "$INSTALL_DIR/"
        chmod +x "$INSTALL_DIR/$BINARY_NAME"
    else
        sudo mv "$tmp_dir/$BINARY_NAME" "$INSTALL_DIR/"
        sudo chmod +x "$INSTALL_DIR/$BINARY_NAME"
    fi
}

# Verify installation
verify_installation() {
    if command -v llmkube &> /dev/null; then
        info "Installation successful!"
        echo ""
        llmkube version
        echo ""
        info "Run 'llmkube --help' to get started"
    else
        warn "Installation complete, but 'llmkube' not found in PATH"
        warn "You may need to add ${INSTALL_DIR} to your PATH"
    fi
}

# Main
main() {
    echo ""
    echo "  _     _     __  __ _  __      _          "
    echo " | |   | |   |  \/  | |/ /     | |         "
    echo " | |   | |   | \  / | ' / _   _| |__   ___ "
    echo " | |   | |   | |\/| |  < | | | | '_ \ / _ \\"
    echo " | |___| |___| |  | | . \| |_| | |_) |  __/"
    echo " |_____|_____|_|  |_|_|\_\\\\__,_|_.__/ \___|"
    echo ""
    echo " GPU-accelerated LLM inference on Kubernetes"
    echo ""

    # Check for Homebrew on macOS
    local os=$(detect_os)
    if [[ "$os" == "darwin" ]] && command -v brew &> /dev/null; then
        warn "Homebrew detected! Consider using: brew install defilantech/tap/llmkube"
        echo ""
    fi

    local arch=$(detect_arch)
    local version="${LLMKUBE_VERSION:-$(get_latest_version)}"

    if [[ -z "$version" ]]; then
        error "Failed to determine latest version. Set LLMKUBE_VERSION manually."
    fi

    info "Detected: ${os}/${arch}"
    info "Version: ${version}"
    echo ""

    download_and_install "$version" "$os" "$arch"
    verify_installation
}

main "$@"
