#!/usr/bin/env bash
# Writes the changelog of release VERSION from the commit history with
# git-cliff (cliff.toml, ADR-0032): the release notes of the commits since
# the last tag into NOTES (default dist/release-notes.md), and CHANGELOG.md
# with that release's entry added on top (the whole history when it does
# not exist yet). GIT_CLIFF names the git-cliff binary.
#
# Before VERSION is tagged its commits are the unreleased ones; when a
# release is run again after its tag was pushed, the tag already names HEAD
# and the commits are the latest release's. CHANGELOG.md gains the entry
# only once.
set -euo pipefail
VERSION="${1:?usage: changelog.sh <vX.Y.Z> [notes file]}"
NOTES="${2:-dist/release-notes.md}"
GIT_CLIFF="${GIT_CLIFF:-git-cliff}"
cd "$(dirname "$0")/.."
mkdir -p "$(dirname "$NOTES")"
if [ "$(git rev-parse -q --verify "refs/tags/${VERSION}^{commit}" || true)" = "$(git rev-parse HEAD)" ]; then
  release=(--latest)
else
  release=(--unreleased --tag "$VERSION")
fi
$GIT_CLIFF --config cliff.toml "${release[@]}" --strip header --output "$NOTES"
if [ ! -f CHANGELOG.md ]; then
  if [ "${release[0]}" = --latest ]; then
    $GIT_CLIFF --config cliff.toml --output CHANGELOG.md
  else
    $GIT_CLIFF --config cliff.toml --tag "$VERSION" --output CHANGELOG.md
  fi
elif ! grep -q "^## ${VERSION} " CHANGELOG.md; then
  $GIT_CLIFF --config cliff.toml "${release[@]}" --prepend CHANGELOG.md
fi
