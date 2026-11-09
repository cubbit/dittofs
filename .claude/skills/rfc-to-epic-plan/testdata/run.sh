#!/bin/sh
# Runs the checker one directory up over every fixture here and diffs its output against the
# fixture's .want. A valid-* fixture must print only its TOTAL line; any other fixture outside
# known/ must print at least one complaint besides it. Usage: [AWK=gawk] ./run.sh
cd "$(dirname "$0")" || exit 2
AWK=${AWK:-awk}
CHECK=${CHECK:-$(ls ../*-check.awk)}
fail=0
for f in *.md known/*.md; do
  [ -f "$f" ] || continue
  want=${f%.md}.want
  if ! out=$("$AWK" -f "$CHECK" "$f" | diff "$want" -); then
    printf 'FAIL %s\n%s\n' "$f" "$out"; fail=1
  fi
  extra=$(grep -vc '^TOTAL' "$want")
  case $f in
    known/*) ;;
    valid-*) [ "$extra" -eq 0 ] || { echo "FAIL $f: a valid fixture's .want has complaints"; fail=1; } ;;
    *) [ "$extra" -gt 0 ] || { echo "FAIL $f: a defect fixture's .want has no complaint"; fail=1; } ;;
  esac
done
[ $fail -eq 0 ] && echo ok
exit $fail
