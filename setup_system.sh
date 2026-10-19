#!/usr/bin/env bash
# Stable entrypoint for Ownstack runners that clone the cluster repo and run
# this script on the server. It installs the pinned ownstackctl (install.sh)
# and applies ownstack.yaml from this repo. Arguments are passed to apply.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

install_dir="${OWNSTACKCTL_INSTALL_DIR:-/usr/local/bin}"
OWNSTACKCTL_INSTALL_DIR="$install_dir" bash ./install.sh
exec "$install_dir/ownstackctl" apply "$@"
