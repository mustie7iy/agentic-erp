#!/usr/bin/env bash
# Installs, for scripts/ci-local.sh, the tools GitHub's deploy workflow and
# release-scripts job install (06 §6.5), into DIR (default
# $HOME/.cache/ci-tools/bin, beside scripts/ci-tools.sh's), and prints DIR.
#
# The versions are read from the workflows themselves, so the gate a pull
# request passes on this machine is the gate main's run applies: a version
# bumped there is installed here. Released binaries are checked against a
# hash: on linux/amd64 the one the workflow pins, elsewhere the one recorded
# below from the project's own checksum file. A version with no hash
# recorded for this platform fails rather than installs unchecked, so a bump
# in a workflow is a bump here too. The Go-built tools are the `go install`
# lines of deploy.yml, at their versions.
set -euo pipefail
cd "$(dirname "$0")/.."
DIR="${1:-$HOME/.cache/ci-tools/bin}"
mkdir -p "$DIR"
os="$(go env GOOS)"
arch="$(go env GOARCH)"
platform="${os}_${arch}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# platform tool version algorithm:hash, from each release's checksum file.
hashes='
darwin_arm64 terraform 1.16.4 256:42cfdf97ad722f79085fe2279b06d4b8680172de3534b22eeddd9a0fbbe7b8f1
darwin_arm64 actionlint 1.7.12 256:aba9ced2dee8d27fecca3dc7feb1a7f9a52caefa1eb46f3271ea66b6e0e6953f
darwin_arm64 helm 4.1.4 256:7c2eca678e8001fa863cdf8cbf6ac1b3799f9404a89eb55c08260ef5732e658d
darwin_arm64 kubeconform 0.8.0 256:f84f4dfbebf4a6b0b230385fa065a39ea35e02608c2b50d025dcf64775a69d67
darwin_arm64 kyverno 1.19.1 256:2cf5febbedaafca5d3c7819925a49f9723bf4c3337a1327b6f72970480af385a
darwin_arm64 zot 2.1.22 256:c81a26040ae79c7e8a90c306a2377c69982a86ff35c259427bd9accea0eca14c
darwin_arm64 syft 1.52.0 256:014d561b6d13059124155f74a6c5a9a99501f5e209313638dd884f39eb418ee6
darwin_arm64 promtool 3.15.0 256:920df4d17e78b3b0175af144eb318b0c74d1cf7b1d1251b326966f0e81977260
darwin_arm64 git-cliff 2.14.2 512:6ec30660233fcd73b92e9d888965945b6435d6fcd7120df8d7c0799faa46cf06d563d49de721ccf4387e6d61bdc5b37c8eb73273c7d9a0883b90cd9857c6ad9f
'

# pin WORKFLOW VAR prints the value WORKFLOW's env sets VAR to.
pin() {
  local v
  v="$(sed -n "s/^ *$2: *//p" ".github/workflows/$1" | head -1)"
  [ -n "$v" ] || { echo "ci-local-tools: $1 sets no $2" >&2; return 1; }
  echo "$v"
}

# want TOOL VERSION WORKFLOW VAR ALGORITHM prints algorithm:hash for this platform.
want() {
  local h
  if [ "$platform" = linux_amd64 ]; then
    h="$(pin "$3" "${4}_SHA$5")"
    echo "$5:$h"
    return
  fi
  h="$(awk -v p="$platform" -v t="$1" -v v="$2" '$1 == p && $2 == t && $3 == v {print $4}' <<<"$hashes")"
  [ -n "$h" ] || { echo "ci-local-tools: no hash recorded for $1 $2 on $platform; add it from the release's checksum file" >&2; return 1; }
  echo "$h"
}

# fetch TOOL VERSION URL MEMBER WORKFLOW VAR ALGORITHM installs MEMBER of
# the archive at URL (the file itself when it is not an archive) as
# DIR/TOOL, once its hash is the one pinned for this platform.
fetch() {
  local tool="$1" v="$2" url="$3" member="$4" stamp="$1 $2 $platform" hash
  hash="$(want "$tool" "$v" "$5" "$6" "$7")"
  if [ -x "$DIR/$tool" ] && [ "$(cat "$DIR/.$tool.version" 2>/dev/null || true)" = "$stamp" ]; then
    return 0
  fi
  rm -rf "$work/x" "$work/archive" && mkdir "$work/x"
  curl -sSfL --retry 3 -o "$work/archive" "$url"
  if ! echo "${hash#*:}  $work/archive" | shasum -a "${hash%%:*}" -c - >/dev/null; then
    echo "ci-local-tools: $url does not have the pinned hash" >&2
    return 1
  fi
  case "$url" in
    *.zip) unzip -q "$work/archive" "$member" -d "$work/x" ;;
    *.tar.gz | *.tgz) tar -xzf "$work/archive" -C "$work/x" "$member" ;;
    *) cp "$work/archive" "$work/x/$member" ;;
  esac
  install -m 0755 "$work/x/$member" "$DIR/$tool"
  echo "$stamp" >"$DIR/.$tool.version"
}

d=deploy.yml
v="$(pin $d TERRAFORM_VERSION)"
fetch terraform "$v" "https://releases.hashicorp.com/terraform/$v/terraform_${v}_${os}_${arch}.zip" terraform $d TERRAFORM 256
v="$(pin $d ACTIONLINT_VERSION)"
fetch actionlint "$v" "https://github.com/rhysd/actionlint/releases/download/v$v/actionlint_${v}_${os}_${arch}.tar.gz" actionlint $d ACTIONLINT 256
v="$(pin $d HELM_VERSION)"
fetch helm "$v" "https://get.helm.sh/helm-v$v-$os-$arch.tar.gz" "$os-$arch/helm" $d HELM 256
v="$(pin $d KUBECONFORM_VERSION)"
fetch kubeconform "$v" "https://github.com/yannh/kubeconform/releases/download/v$v/kubeconform-$os-$arch.tar.gz" kubeconform $d KUBECONFORM 256
v="$(pin $d KYVERNO_VERSION)"
karch="$arch"
[ "$arch" != amd64 ] || karch=x86_64
fetch kyverno "$v" "https://github.com/kyverno/kyverno/releases/download/v$v/kyverno-cli_v${v}_${os}_$karch.tar.gz" kyverno $d KYVERNO 256
v="$(pin $d ZOT_VERSION)"
fetch zot "$v" "https://github.com/project-zot/zot/releases/download/v$v/zot-$os-$arch-minimal" zot $d ZOT 256
v="$(pin $d SYFT_VERSION)"
fetch syft "$v" "https://github.com/anchore/syft/releases/download/v$v/syft_${v}_${os}_$arch.tar.gz" syft $d SYFT 256
v="$(pin $d PROMTOOL_VERSION)"
fetch promtool "$v" "https://github.com/prometheus/prometheus/releases/download/v$v/prometheus-$v.$os-$arch.tar.gz" "prometheus-$v.$os-$arch/promtool" $d PROMTOOL 256
v="$(pin ci.yml GIT_CLIFF_VERSION)"
case "$platform" in
  darwin_arm64) triple=aarch64-apple-darwin ;;
  *) triple=x86_64-unknown-linux-gnu ;;
esac
fetch git-cliff "$v" "https://github.com/orhun/git-cliff/releases/download/v$v/git-cliff-$v-$triple.tar.gz" "git-cliff-$v/git-cliff" ci.yml GIT_CLIFF 512

# The Go-built tools, as deploy.yml installs them, but those
# scripts/ci-tools.sh pins (golangci-lint): its stamp and version are its own.
for module in $(grep -o 'go install [^ ]*@v[^ ]*' .github/workflows/deploy.yml | awk '{print $3}' | sort -u); do
  name="$(basename "${module%@*}")"
  if grep -q "^$name " scripts/ci-tools.sh; then
    grep -q "^$name $module\$" scripts/ci-tools.sh || { echo "ci-local-tools: deploy.yml installs $module, scripts/ci-tools.sh another $name" >&2; exit 1; }
    continue
  fi
  if [ ! -x "$DIR/$name" ] || [ "$(cat "$DIR/.$name.version" 2>/dev/null || true)" != "$module" ]; then
    GOBIN="$DIR" go install "$module"
    echo "$module" >"$DIR/.$name.version"
  fi
done
echo "$DIR"
