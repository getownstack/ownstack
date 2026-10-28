#!/usr/bin/env bash
# Installs ownstackctl from this repository's GitHub releases after verifying
# its checksum.
#
#   curl -fsSL https://raw.githubusercontent.com/getownstack/ownstack/main/install.sh | sudo bash
#
# Environment:
#   OWNSTACKCTL_VERSION      release tag, such as ownstackctl-v0.4.0 (default: the latest release)
#   OWNSTACKCTL_INSTALL_DIR  where to put the binary (default /usr/local/bin)
set -euo pipefail

version="${OWNSTACKCTL_VERSION:-latest}"
install_dir="${OWNSTACKCTL_INSTALL_DIR:-/usr/local/bin}"

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64|Linux-amd64) arch="amd64" ;;
  Linux-aarch64|Linux-arm64) arch="arm64" ;;
  *) echo "ownstackctl runs on Linux amd64 or arm64 servers; this is $(uname -s) $(uname -m)" >&2; exit 1 ;;
esac

asset="ownstackctl-linux-$arch"
if [ "$version" = latest ]; then
  base="https://github.com/getownstack/ownstack/releases/latest/download"
else
  base="https://github.com/getownstack/ownstack/releases/download/$version"
fi
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $asset ($version)"
for file in "$asset" checksums.txt; do
  curl -fsSL --proto '=https' --retry 3 --retry-connrefused "$base/$file" -o "$tmp/$file"
done
# checksums.txt lists every binary of the release; verify the one downloaded.
(cd "$tmp" && grep "  $asset\$" checksums.txt | sha256sum -c --quiet -)

mkdir -p "$install_dir"
install -m 0755 "$tmp/$asset" "$install_dir/ownstackctl"
echo "Installed $("$install_dir/ownstackctl" version) to $install_dir/ownstackctl"
