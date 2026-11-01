# shellcheck shell=bash
# The pinned upstream images of deploy/images/upstream.txt (ADR-0035), read
# the same way by the release's copy (scripts/mirror-images.sh) and its
# scan (scripts/image-scan.sh), so the scan covers exactly what is copied.
#
# upstream_platform is the one platform copied and scanned: the cells'
# nodes are Graviton (modules/cell).
# shellcheck disable=SC2034 # read by the scripts that source this file
upstream_platform=linux/arm64

# upstream_images LIST: prints each image of LIST as "name source tag
# digest", one per line, skipping comments and blank lines. Fails, naming
# the caller, on a name that is not a repository name, a digest that is not
# a sha256 digest, or a list with no image: a list in another shape would
# otherwise copy or scan nothing. Call it in an assignment
# (list="$(upstream_images "$LIST")"), whose status set -e sees; a process
# substitution's it does not.
upstream_images() {
  local name source tag digest found=0 me="${0##*/}"
  me="${me%.sh}"
  while read -r name source tag digest; do
    case "$name" in '' | '#'*) continue ;; esac
    if ! [[ "$name" =~ ^[a-z][a-z0-9-]{0,40}$ ]]; then
      echo "$me: $name is not a repository name (lower-case letters, digits, hyphens)" >&2
      return 1
    fi
    if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
      echo "$me: $name: $digest is not a sha256 digest" >&2
      return 1
    fi
    echo "$name $source $tag $digest"
    found=1
  done <"$1"
  if [ "$found" -eq 0 ]; then
    echo "$me: $1 lists no image" >&2
    return 1
  fi
}
