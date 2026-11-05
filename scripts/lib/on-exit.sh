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
# status 0 before it is a stop, and exits 1 with the reason. bash 3.2
# runs the EXIT trap of a shell a signal stops with $? 0 too, then exits
# 128 plus the signal's number; the reason names that cause as well. The
# lib traps no signal: a trapped one waits for the foreground command to
# end, and a gate's go test or build may run for minutes.
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
    echo "$(basename "$0"): stopped before its end with status 0 (an unbound variable or another expansion error reported above, an exit 0 before finished, or a signal, which sets the exit status itself): exits 1" >&2
    status=1
  fi
  exit "$status"
}

# on_substitution_exit CMD runs `CMD STATUS` when a command substitution
# of the script exits, with its status: bash runs no EXIT trap of the
# script's there, so an unbound variable that stops $(f) is seen by nobody
# when the caller drops the substitution's status (scripts/walkthrough.sh
# stop_unseen). A DEBUG trap, which set -T passes on to subshells, sets
# the EXIT trap before a substitution's first command. Only there: bash
# clears set -e in a substitution alone, and bash 3.2 gives any other
# subshell with an EXIT trap, ( ... ) or a pipeline's function, status 0
# after an expansion error, where without one it exits 1 and set -e stops
# the script (review:T61j round 1).
ON_SUBSTITUTION_CMD=''
ON_SUBSHELL=0

on_substitution_exit() {
  ON_SUBSTITUTION_CMD=$1
  set -T
  trap '[ "$BASH_SUBSHELL" = "$ON_SUBSHELL" ] || on_subshell_enter' DEBUG
}

on_subshell_enter() {
  ON_SUBSHELL=$BASH_SUBSHELL
  case $- in
  *e*) ;;
  *) trap '$ON_SUBSTITUTION_CMD $?' EXIT ;;
  esac
}
