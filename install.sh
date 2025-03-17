#!/bin/sh

# KubeVPN installation script
# This script installs KubeVPN CLI to your system
# Created for https://github.com/kubenetworks/kubevpn
# curl -fsSL https://kubevpn.dev/install.sh | sh

set -e

# Colors and formatting
YELLOW='\033[0;33m'
GREEN='\033[0;32m'
RED='\033[0;31m'
BLUE='\033[0;34m'
BOLD='\033[1m'
RESET='\033[0m'

# Installation configuration
INSTALL_DIR=${INSTALL_DIR:-"/usr/local/bin"}
GITHUB_REPO="kubenetworks/kubevpn"
GITHUB_URL="https://github.com/${GITHUB_REPO}"
VERSION_URL="https://raw.githubusercontent.com/kubenetworks/kubevpn/refs/heads/master/plugins/stable.txt"
ZIP_FILE="kubevpn.zip"

log() {
  echo "${BLUE}${BOLD}==> ${RESET}$1"
}

success() {
  echo "${GREEN}${BOLD}==> $1${RESET}"
}

warn() {
  echo "${YELLOW}${BOLD}==> $1${RESET}"
}

error() {
  echo "${RED}${BOLD}==> $1${RESET}"
}

get_system_info() {
  OS=$(uname | tr '[:upper:]' '[:lower:]')
  log "Detected OS: ${OS}"

  case $OS in
  linux | darwin) ;;
  msys_nt | msys | cygwin)
    error "Windows is not supported, please install KubeVPN manually using scoop. More info: ${GITHUB_URL}"
    exit 1
    ;;
  *)
    error "Unsupported operating system: ${OS}"
    exit 1
    ;;
  esac

  ARCH=$(uname -m)
  case $ARCH in
  x86_64)
    ARCH="amd64"
    ;;
  aarch64 | arm64)
    ARCH="arm64"
    ;;
  i386 | i686)
    ARCH="386"
    ;;
  *)
    error "Unsupported architecture: ${ARCH}"
    exit 1
    ;;
  esac
  log "Detected architecture: ${ARCH}"
}

check_requirements() {
  if command -v curl >/dev/null 2>&1; then
    DOWNLOADER="curl"
  elif command -v wget >/dev/null 2>&1; then
    DOWNLOADER="wget"
  else
    error "Either curl or wget is required for installation"
    exit 1
  fi

  if ! command -v unzip >/dev/null 2>&1; then
    error "unzip is required but not installed"
    exit 1
  fi

  if [ ! -d "$INSTALL_DIR" ]; then
    log "Installation directory $INSTALL_DIR does not exist, attempting to create it"
    if ! mkdir -p "$INSTALL_DIR" 2>/dev/null; then
      if ! command -v sudo >/dev/null 2>&1 && ! command -v su >/dev/null 2>&1; then
        error "Cannot create $INSTALL_DIR and neither sudo nor su is available"
        exit 1
      fi
    fi
  fi

  if [ ! -w "$INSTALL_DIR" ] && ! command -v sudo >/dev/null 2>&1 && ! command -v su >/dev/null 2>&1; then
    error "No write permission to $INSTALL_DIR and neither sudo nor su is available"
    exit 1
  fi
}

get_latest_version() {
  log "Fetching the latest release version..."

  if [ "$DOWNLOADER" = "curl" ]; then
    VERSION=$(curl -s "$VERSION_URL")
  else
    VERSION=$(wget -qO- "$VERSION_URL")
  fi

  if [ -z "$VERSION" ]; then
    error "Could not determine the latest version"
    exit 1
  fi

  VERSION=$(echo "$VERSION" | tr -d 'v' | tr -d '\n')
  success "Latest version: ${VERSION}"
}

download_binary() {
  DOWNLOAD_URL="$GITHUB_URL/releases/download/v${VERSION}/kubevpn_v${VERSION}_${OS}_${ARCH}.zip"

  log "Downloading KubeVPN binary from $DOWNLOAD_URL"

  if [ "$DOWNLOADER" = "curl" ]; then
    curl -L -o "$ZIP_FILE" "$DOWNLOAD_URL" || {
      error "Failed to download KubeVPN"
      exit 1
    }
  else
    wget -O "$ZIP_FILE" "$DOWNLOAD_URL" || {
      error "Failed to download KubeVPN"
      exit 1
    }
  fi
}

install_binary() {
  log "Installing KubeVPN..."

  TMP_DIR=$(mktemp -d)
  BINARY="$TMP_DIR/bin/kubevpn"
  unzip -o -q "$ZIP_FILE" -d "$TMP_DIR"

  if [ -f "$TMP_DIR/checksums.txt" ]; then
    EXPECTED_CHECKSUM=$(cat "$TMP_DIR/checksums.txt" | awk '{print $1}')

    if command -v shasum >/dev/null 2>&1; then
      ACTUAL_CHECKSUM=$(shasum -a 256 "$BINARY" | awk '{print $1}')
    elif command -v sha256sum >/dev/null 2>&1; then
      ACTUAL_CHECKSUM=$(sha256sum "$BINARY" | awk '{print $1}')
    else
      warn "No checksum tool available, skipping verification"
      ACTUAL_CHECKSUM=$EXPECTED_CHECKSUM
    fi

    [ "$ACTUAL_CHECKSUM" = "$EXPECTED_CHECKSUM" ] || {
      error "Checksum verification failed (Expected: $EXPECTED_CHECKSUM, Got: $ACTUAL_CHECKSUM)"
      # Clean up
      rm -rf "$TMP_DIR"
      rm -f "$ZIP_FILE"
      exit 1
    }
  fi

  # Check if we need sudo
  if [ -w "$INSTALL_DIR" ]; then
    mv "$BINARY" "$INSTALL_DIR/kubevpn"
    chmod +x "$INSTALL_DIR/kubevpn"
  else
    warn "Elevated permissions required to install to $INSTALL_DIR"
    if command -v sudo >/dev/null 2>&1; then
      sudo mv "$BINARY" "$INSTALL_DIR/kubevpn"
      sudo chmod +x "$INSTALL_DIR/kubevpn"
    else
      su -c "mv \"$BINARY\" \"$INSTALL_DIR/kubevpn\" && chmod +x \"$INSTALL_DIR/kubevpn\""
    fi
  fi

  # Clean up
  rm -f "$ZIP_FILE"
  rm -rf "$TMP_DIR"
}

verify_installation() {
  if [ -x "$INSTALL_DIR/kubevpn" ]; then
    VERSION_OUTPUT=$("$INSTALL_DIR/kubevpn" version 2>&1 || echo "unknown")
    success "KubeVPN installed successfully"
    log "$VERSION_OUTPUT"
    log "KubeVPN has been installed to: $INSTALL_DIR/kubevpn"

    # Check if the installed binary is in PATH
    if command -v kubevpn >/dev/null 2>&1; then
      FOUND_PATH=$(command -v kubevpn)
      if [ "$FOUND_PATH" != "$INSTALL_DIR/kubevpn" ]; then
        warn "Another kubevpn binary was found in your PATH at: $FOUND_PATH"
        warn "Make sure $INSTALL_DIR is in your PATH to use the newly installed version"
      fi
    else
      warn "Make sure $INSTALL_DIR is in your PATH to use kubevpn"
    fi

    echo ""
    log "To connect to a Kubernetes cluster:"
    if [ "$FOUND_PATH" != "$INSTALL_DIR/kubevpn" ]; then
      echo "  $INSTALL_DIR/kubevpn connect"
    else
      echo "  kubevpn connect"
    fi
    echo ""
    log "For more information, visit:"
    echo "  $GITHUB_URL"
    success "Done! enjoy KubeVPN 🚀"
  else
    error "KubeVPN installation failed"
    exit 1
  fi
}

main() {
  log "Starting KubeVPN installation..."
  get_system_info
  check_requirements
  get_latest_version
  download_binary
  install_binary
  verify_installation
}

main
