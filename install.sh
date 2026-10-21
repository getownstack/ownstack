#!/usr/bin/env bash
# Installs ownstackctl from a pinned GitHub Release after verifying its checksum.
#
#   curl -fsSL https://raw.githubusercontent.com/getownstack/ownstack-cluster-template/main/install.sh | sudo bash
#
# Environment:
#   OWNSTACKCTL_VERSION      release tag (default below)
#   OWNSTACKCTL_REPO         owner/repo hosting the releases
#   OWNSTACKCTL_INSTALL_DIR  where to put the binary (default /usr/local/bin)
set -euo pipefail

version="${OWNSTACKCTL_VERSION:-ownstackctl-v0.3.1}"
repo="${OWNSTACKCTL_REPO:-getownstack/ownstack-cluster-template}"
install_dir="${OWNSTACKCTL_INSTALL_DIR:-/usr/local/bin}"

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64|Linux-amd64) arch="amd64" ;;
  Linux-aarch64|Linux-arm64) arch="arm64" ;;
  *) echo "ownstackctl runs on Linux amd64 or arm64 servers; this is $(uname -s) $(uname -m)" >&2; exit 1 ;;
esac

asset="ownstackctl-linux-$arch"
base="https://github.com/$repo/releases/download/$version"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $asset ($version)"
curl -fsSL --proto '=https' --retry 3 --retry-connrefused "$base/$asset" -o "$tmp/$asset"
curl -fsSL --proto '=https' --retry 3 --retry-connrefused "$base/checksums-linux-$arch.txt" -o "$tmp/checksums.txt"
(cd "$tmp" && sha256sum -c checksums.txt --ignore-missing --quiet)

mkdir -p "$install_dir"
install -m 0755 "$tmp/$asset" "$install_dir/ownstackctl"
echo "Installed $("$install_dir/ownstackctl" version) to $install_dir/ownstackctl"
