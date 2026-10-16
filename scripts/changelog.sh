#!/usr/bin/env bash
# Writes the changelog of release VERSION from the commit history with
# git-cliff (cliff.toml, ADR-0032): the release notes of the commits since
# the last tag into NOTES (default dist/release-notes.md), and CHANGELOG.md
# with that release's entry added on top (the whole history when it does
# not exist yet). GIT_CLIFF names the git-cliff binary.
set -euo pipefail
VERSION="${1:?usage: changelog.sh <vX.Y.Z> [notes file]}"
NOTES="${2:-dist/release-notes.md}"
GIT_CLIFF="${GIT_CLIFF:-git-cliff}"
cd "$(dirname "$0")/.."
mkdir -p "$(dirname "$NOTES")"
$GIT_CLIFF --config cliff.toml --unreleased --tag "$VERSION" --strip header --output "$NOTES"
if [ -f CHANGELOG.md ]; then
  $GIT_CLIFF --config cliff.toml --unreleased --tag "$VERSION" --prepend CHANGELOG.md
else
  $GIT_CLIFF --config cliff.toml --tag "$VERSION" --output CHANGELOG.md
fi
