#!/usr/bin/env bash
# Stable entrypoint for Ownstack runners that clone the cluster repo and run
# this script on the server. It installs the pinned ownstackctl and applies
# ownstack.yaml from this repo (or, for older runners, the legacy lowercase
# environment variables).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$repo_root"

if [ -x ./bin/ownstackctl ]; then
  exec ./bin/ownstackctl apply "$@"
fi

if [ -n "${OWNSTACKCTL_URL:-}" ]; then
  mkdir -p ./bin
  echo "Downloading ownstackctl from OWNSTACKCTL_URL"
  curl -fsSL "$OWNSTACKCTL_URL" -o ./bin/ownstackctl
  chmod +x ./bin/ownstackctl
  exec ./bin/ownstackctl apply "$@"
fi

if OWNSTACKCTL_INSTALL_DIR="$repo_root/bin" bash ./install.sh; then
  exec ./bin/ownstackctl apply "$@"
fi

if command -v go >/dev/null 2>&1; then
  exec go run ./cmd/ownstackctl apply "$@"
fi

# Deprecated: the pre-ownstackctl shell installer only understands the legacy
# environment contract. Remove once ownstackctl v0.2 is verified in the field.
if [ -n "${vps:-}" ]; then
  echo "ownstackctl is unavailable; falling back to the legacy shell installer." >&2
  exec bash ./scripts/setup_system_legacy.sh
fi

echo "ownstackctl could not be installed; see the errors above." >&2
exit 1
