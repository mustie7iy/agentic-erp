#!/usr/bin/env bash
# Installs the tools CI runs, at pinned versions, into DIR (default
# $HOME/.cache/ci-tools/bin), building only those not there yet, and prints
# DIR. CI caches DIR by this file's hash and the Go version, so a run builds
# them once per change instead of compiling each with `go run` every time
# (06 §6.5). Pinned rather than @latest so the gate a pull request passes is
# the gate the next one meets; a version change is a reviewed change.
set -euo pipefail
DIR="${1:-$HOME/.cache/ci-tools/bin}"
mkdir -p "$DIR"
while read -r name module; do
  [ -n "$name" ] || continue
  if [ ! -x "$DIR/$name" ]; then
    GOBIN="$DIR" go install "$module"
  fi
done <<'EOF'
golangci-lint github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0
staticcheck honnef.co/go/tools/cmd/staticcheck@v0.8.2
govulncheck golang.org/x/vuln/cmd/govulncheck@v1.9.0
gosec github.com/securego/gosec/v2/cmd/gosec@v2.29.0
gitleaks github.com/zricethezav/gitleaks/v8@v8.30.1
vacuum github.com/daveshanley/vacuum@v0.32.0
oasdiff github.com/oasdiff/oasdiff@v1.33.0
EOF
echo "$DIR"
