# on_exit CMD runs CMD when the script exits, as `trap CMD EXIT` would, and
# keeps a stop the shell did not ask for from exiting 0. macOS's /bin/bash
# (3.2, which `#!/usr/bin/env bash` finds there) stops on an expansion
# error (an unbound variable under set -u, ${x:?}, an empty "${a[@]}") and
# runs the EXIT trap with $? 0, so the script exited 0 after stopping
# halfway: `make walkthrough` passed a run that stopped on an unbound
# variable (review:T61j, 06 §6.5). bash 5 keeps the status; this works
# the same on both.
#
# A script that sources this calls `finished` once it has done all its
# work, at its end and before every deliberate `exit 0`: an exit with
# status 0 before it is a stop, and exits 1 with the reason.
#
# shellcheck shell=bash
ON_EXIT_CMD=''
ON_EXIT_DONE=0

on_exit() {
  ON_EXIT_CMD=$1
  trap 'on_exit_run $?' EXIT
}

finished() { ON_EXIT_DONE=1; }

on_exit_run() {
  local status=$1
  eval "$ON_EXIT_CMD"
  if [ "$status" = 0 ] && [ "$ON_EXIT_DONE" != 1 ]; then
    echo "$(basename "$0"): stopped before its end with status 0 (an unbound variable or another expansion error reported above, or an exit 0 before finished): exits 1" >&2
    status=1
  fi
  exit "$status"
}
