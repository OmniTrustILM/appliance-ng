#!/usr/bin/env bash
# Renders and downloads the add-on manifests into addons/ and pins their sha256 in release.yaml.
# Helm charts are rendered at the versions release.yaml gives; the other files come from the
# URLs in addons/src/downloads.txt. The output carries no timestamps, so a second run changes nothing.
set -euo pipefail
export LC_ALL=C

cd "$(dirname "$0")/.."
readonly release=release.yaml addons=addons src=addons/src
readonly cilium_repo=https://helm.cilium.io traefik_repo=https://traefik.github.io/charts

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

die() { echo "addons: $*" >&2; exit 1; }

# field <file> <key> prints the value of <key> in the release.yaml add-on entry of <file>.
field() {
  local file="$1" key="$2"
  awk -v file="$file" -v key="$key" '
    index($0, "file: " file ",") && match($0, key ": \"[^\"]*\"") {
      value = substr($0, RSTART + length(key) + 3, RLENGTH - length(key) - 4)
      found = 1
      print value
      exit
    }
    END { if (!found) exit 1 }' "$release" || die "$release has no $key for $file"
}

check_source() {
  local file="$1" want="$2"
  [[ "$(field "$file" source)" == "$want" ]] || die "$release: source of $file must be $want"
}

sha256() {
  local file="$1"
  if command -v sha256sum >/dev/null; then sha256sum "$file"; else shasum -a 256 "$file"; fi | cut -d' ' -f1
}

# helm_template <file> <repo> <chart> <namespace> <values> renders a chart into addons/<file>.
helm_template() {
  local file="$1" repo="$2" chart="$3" namespace="$4" values="$5" version
  version="$(field "$file" version)"
  check_source "$file" "helm:$repo/$chart@$version"
  echo "rendering $file from $chart $version"
  helm template "$chart" "$chart" --repo "$repo" --version "$version" --namespace "$namespace" \
    --kube-version "$kube_version" -f "$values" >"$work/$file"
}

kube_version="$(awk '$1 == "kubernetes:" { print $2; exit }' "$release")"
[[ -n "$kube_version" ]] || die "$release has no talos.kubernetes"

helm_template cilium.yaml "$cilium_repo" cilium kube-system "$src/cilium-values.yaml"
mv "$work/cilium.yaml" "$addons/cilium.yaml"

# helm template creates no namespace, so traefik.yaml carries its own.
helm_template traefik.yaml "$traefik_repo" traefik traefik "$src/traefik-values.yaml"
printf -- '---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: traefik\n' | cat - "$work/traefik.yaml" >"$addons/traefik.yaml"

version="$(field traefik-crds.yaml version)"
check_source traefik-crds.yaml "helm:$traefik_repo/traefik@$version"
helm pull traefik --repo "$traefik_repo" --version "$version" --untar --untardir "$work/chart"
awk 'FNR == 1 && !/^---$/ { print "---" } { print }' "$work"/chart/traefik/crds/traefik.io_*.yaml >"$addons/traefik-crds.yaml"

while read -r file url namespace; do
  case "$file" in '' | '#'*) continue ;; *) ;; esac
  check_source "$file" "$url"
  echo "downloading $file"
  if [[ -n "$namespace" ]]; then
    mkdir -p "$work/$file"
    printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n' "$namespace" >"$work/$file/namespace.yaml"
    printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: %s\nresources:\n  - namespace.yaml\n  - %s\n' \
      "$namespace" "$url" >"$work/$file/kustomization.yaml"
    kubectl kustomize "$work/$file" >"$addons/$file"
  else
    curl -fsSL --proto '=https' --tlsv1.2 --retry 3 "$url" -o "$addons/$file"
  fi
done <"$src/downloads.txt"

# local-path-provisioner stores volumes on the Talos user volume and is the default StorageClass.
awk '
  { gsub("/opt/local-path-provisioner", "/var/mnt/data/local-path"); print }
  /^kind: StorageClass$/ { storageClass = 1 }
  storageClass && /^  name: local-path$/ {
    print "  annotations:"
    print "    storageclass.kubernetes.io/is-default-class: \"true\""
    storageClass = 0
  }' "$addons/local-path-storage.yaml" >"$work/local-path-storage.yaml"
mv "$work/local-path-storage.yaml" "$addons/local-path-storage.yaml"

awk 'match($0, /file: [^,]+,/) { print substr($0, RSTART + 6, RLENGTH - 7) }' "$release" >"$work/files"
while read -r file; do
  [[ -f "$addons/$file" ]] || die "$release lists $file, which is not in $addons"
  awk -v file="$file" -v sum="$(sha256 "$addons/$file")" '
    index($0, "file: " file ",") { sub(/sha256: "[^"]*"/, "sha256: \"" sum "\"") }
    { print }' "$release" >"$work/release.yaml"
  cat "$work/release.yaml" >"$release"
done <"$work/files"
echo "add-ons written to $addons, sha256 pinned in $release"
