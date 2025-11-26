#!/bin/bash
# LLMKube Installation Script
# Usage: curl -sSL https://raw.githubusercontent.com/defilantech/LLMKube/main/install.sh | bash
#
# Options (via environment variables):
#   LLMKUBE_VERSION  - Install specific version (default: latest)
#   LLMKUBE_INSTALL_DIR - Installation directory (default: /usr/local/bin)
#   LLMKUBE_NO_SUDO  - Set to 1 to skip sudo (for user-local installs)

set -e

REPO="defilantech/LLMKube"
BINARY_NAME="llmkube"
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

# Download and install
download_and_install() {
    local version="$1"
    local os="$2"
    local arch="$3"

    # Remove 'v' prefix for filename
    local version_num="${version#v}"

    # Construct download URL
    local filename="${BINARY_NAME}_${version_num}_${os}_${arch}.tar.gz"
    local url="https://github.com/${REPO}/releases/download/${version}/${filename}"

    info "Downloading llmkube ${version} for ${os}/${arch}..."

    # Create temp directory
    local tmp_dir=$(mktemp -d)
    trap "rm -rf $tmp_dir" EXIT

    # Download
    if command -v curl &> /dev/null; then
        curl -sSL "$url" -o "$tmp_dir/llmkube.tar.gz" || error "Failed to download from $url"
    else
        wget -q "$url" -O "$tmp_dir/llmkube.tar.gz" || error "Failed to download from $url"
    fi

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
