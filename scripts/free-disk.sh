#!/usr/bin/env bash
# Frees disk on a GitHub-hosted runner before the image builds (ADR-0032):
# the runner's 72 GB disk starts with about 8 GB free once the suite has
# run, and two clean multi-platform ko builds with private caches need more
# (run 39031819702 died of it). It removes preinstalled toolchains the
# workflows never use and the Docker images no job container needs, and
# prints the free space before and after. Only on a CI runner: elsewhere it
# refuses, since it deletes system directories.
set -euo pipefail
if [ -z "${GITHUB_ACTIONS:-}" ] && [ "${1:-}" != --dry-run ]; then
  echo "free-disk: only on a GitHub Actions runner (or --dry-run)" >&2
  exit 2
fi
unused=(
  /usr/share/dotnet       # .NET SDKs
  /usr/local/lib/android  # the Android SDK
  /opt/ghc                # Haskell
  /usr/local/.ghcup
  /opt/hostedtoolcache/CodeQL
  /usr/local/share/boost
  /usr/local/share/powershell
  /usr/share/swift
)
echo "free-disk: before: $(df -h / | tail -1)"
if [ "${1:-}" = --dry-run ]; then
  printf 'free-disk: would remove %s\n' "${unused[@]}"
  echo "free-disk: would prune Docker images no container uses"
  exit 0
fi
for dir in "${unused[@]}"; do
  sudo rm -rf "$dir"
done
# Images of running service containers (PostgreSQL, the registry) are in
# use and stay.
docker image prune --all --force >/dev/null
echo "free-disk: after: $(df -h / | tail -1)"
