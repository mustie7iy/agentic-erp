# shellcheck shell=bash
# Dates for scripts/walkthrough.sh, sourced by it and by its test
# (cmd/erp TestWalkthroughDates).
#
# ZONE is the pilot company's time zone, its configuration's time_zone
# (step 1). A date the walkthrough sends as the company's day (a posting
# date, a filing date, a run date) is its date there, as the API judges
# it (the company's time zone, T72n, PO 2026-10-31; ADR-0051 As built in
# T72n1), not UTC's: from 19:00 or 20:00 in Toronto UTC is already
# tomorrow, and a return filed on UTC's date was refused as filed in the
# future (tax.invalid_filed_on).
ZONE=America/Toronto

# company_day [SECONDS] is the company's date in its zone now, or at
# SECONDS since the epoch, with GNU or BSD date.
company_day() {
  if [ -z "${1:-}" ]; then
    TZ=$ZONE date +%F
  elif date -d @0 +%F >/dev/null 2>&1; then
    TZ=$ZONE date -d "@$1" +%F
  else
    TZ=$ZONE date -r "$1" +%F
  fi
}

# date_add DATE DAYS is the date DAYS days after DATE (before it when
# DAYS is negative), with GNU or BSD date: calendar arithmetic, in UTC so
# that no zone's change of clocks moves it.
date_add() {
  local n=$2
  case "$n" in -*) ;; *) n="+$n" ;; esac
  if date -u -d "$1" +%F >/dev/null 2>&1; then
    date -u -d "$1 $n days" +%F
  else
    date -u -j -v"${n}d" -f %F "$1" +%F
  fi
}

# utc_day [SECONDS] is UTC's date now, or at SECONDS since the epoch: the
# database's day (current_date), by which partner judges a business
# partner's dates. company_day in zone UTC.
utc_day() {
  ZONE=UTC company_day "$@"
}
