#!/usr/bin/env bash
set -euo pipefail
# Like cron's crontab: -l fails without a crontab, and - reads all input before replacing it.
case "$1" in
  -l)
    [ -f "$CRONTAB_TEST_FILE" ] || { echo "no crontab for root" >&2; exit 1; }
    cat "$CRONTAB_TEST_FILE"
    ;;
  -)
    supplied=$(cat; echo x)
    printf '%s' "${supplied%x}" > "$CRONTAB_TEST_FILE"
    ;;
  *) exit 3 ;;
esac
