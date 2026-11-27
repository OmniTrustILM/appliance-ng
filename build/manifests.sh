#!/usr/bin/env bash
# Writes addons/manager.yaml from the generated Appliance CRD and addons/src/manager.yaml, and pins its sha256 in
# release.yaml. The CRD comes first, so that the manager's DaemonSet is the last document.
set -euo pipefail
export LC_ALL=C

cd "$(dirname "$0")/.."
readonly release=release.yaml manifest=addons/manager.yaml

cat config/crd/appliance.otilm.com_appliances.yaml addons/src/manager.yaml >"$manifest"

if command -v sha256sum >/dev/null; then sum="$(sha256sum "$manifest")"; else sum="$(shasum -a 256 "$manifest")"; fi
sum="${sum%% *}"
grep -q 'file: manager.yaml,' "$release" || { echo "manifests: $release has no add-on entry for manager.yaml" >&2; exit 1; }
work="$(mktemp)"
trap 'rm -f -- "$work"' EXIT
awk -v sum="$sum" 'index($0, "file: manager.yaml,") { sub(/sha256: "[^"]*"/, "sha256: \"" sum "\"") } { print }' "$release" >"$work"
cat "$work" >"$release"
