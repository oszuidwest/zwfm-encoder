#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  -help)
    [ "$AUTH_TEST_MODE" != "help-fail" ] || exit 1
    if [ "$AUTH_TEST_MODE" != "legacy" ]; then
      printf '%s\n' '  -hash-password'
    fi
    ;;
  -hash-password)
    [ "$AUTH_TEST_MODE" != "fail" ] || exit 1
    supplied=$(</dev/stdin)
    [ "$supplied" = "$AUTH_TEST_PASSWORD" ] || exit 2
    printf '%s\n' "$AUTH_TEST_HASH"
    ;;
  *) exit 3 ;;
esac
