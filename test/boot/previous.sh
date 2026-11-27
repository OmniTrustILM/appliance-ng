#!/usr/bin/env bash
# Builds the qcow2 of the appliance the system update test starts from into out/<arch>/previous/: that of the tag
# <from>, or with test that of this commit stamped 0.0.0-test, while a release to start from does not exist yet.
#
#   previous.sh <tag|test> <arch>
#
# It runs make images in a throw-away git worktree, with the version edit there only, and removes the worktree
# afterwards. Its images go to REGISTRY like those of this worktree and its manager trusts BUNDLE_TEST_KEY, the key the
# system bundle of the test is signed with, which must be set. The image layer cache of this worktree is shared.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
log() { echo "[previous] $*" >&2; }
die() { log "error: $*"; exit 1; }

usage() { sed -n '5p' "$0" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
from="$1" arch="$2"
[[ -n "${BUNDLE_TEST_KEY:-}" ]] || die "BUNDLE_TEST_KEY is not set"
if [[ "$from" == test ]]; then
  ref=HEAD version=0.0.0-test
else
  git -C "$REPO" rev-parse -q --verify "refs/tags/$from" >/dev/null || die "the tag $from is not in this clone"
  ref="refs/tags/$from"
  version="$(git -C "$REPO" show "$ref:release.yaml" | sed -n 's/^version: *//p')"
fi

work="$(mktemp -d)"
trap 'git -C "$REPO" worktree remove --force "$work/src" 2>/dev/null || true; rm -rf -- "$work"' EXIT
log "building $version for $arch from $from in $work/src"
git -C "$REPO" worktree add -q --detach "$work/src" "$ref"
if [[ "$from" == test ]]; then
  sed -i.orig "s/^version: .*/version: $version/" "$work/src/release.yaml"
  rm -f "$work/src/release.yaml.orig"
fi
if [[ -d "$REPO/out/layer-cache" ]]; then
  mkdir -p "$work/src/out"
  ln -s "$REPO/out/layer-cache" "$work/src/out/layer-cache"
fi
make -C "$work/src" images ARCH="$arch" >&2

name="ilm-appliance-$version-$arch.qcow2"
rm -rf -- "$REPO/out/$arch/previous"
mkdir -p "$REPO/out/$arch/previous"
mv "$work/src/out/$arch/$name" "$REPO/out/$arch/previous/$name"
log "out/$arch/previous/$name"
