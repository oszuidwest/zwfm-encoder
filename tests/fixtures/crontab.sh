#!/usr/bin/env bash
set -euo pipefail
# Like cron's crontab: -l without a crontab prints "no crontab for root" and exits 1,
# and - reads all input before replacing it.
case "$1" in
  -l)
    if [ -n "${CRONTAB_TEST_READ_ERROR:-}" ]; then
      echo "crontabs/root/: fopen: Input/output error" >&2
      exit 1
    fi
    [ -f "$CRONTAB_TEST_FILE" ] || { echo "no crontab for root" >&2; exit 1; }
    cat "$CRONTAB_TEST_FILE"
    ;;
  -)
    if [ -n "${CRONTAB_TEST_WRITE_ERROR:-}" ]; then
      cat > /dev/null
      echo "errors in crontab file, can't install." >&2
      exit 1
    fi
    supplied=$(cat; echo x)
    printf '%s' "${supplied%x}" > "$CRONTAB_TEST_FILE"
    ;;
  *) exit 3 ;;
esac
