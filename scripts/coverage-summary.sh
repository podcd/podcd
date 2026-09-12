#!/usr/bin/env bash
set -euo pipefail

profile=${1:-coverage.out}
markdown=${2:-}

awk -v md="$markdown" '
NR == 1 { next }                       # mode: line
{
  stmts[$1] = $2
  if ($3 > 0) hit[$1] = 1
}
END {
  for (key in stmts) {
    split(key, loc, ":"); pkg = loc[1]
    sub(/\/[^\/]*$/, "", pkg); sub(/^github\.com\/podcd\/podcd\//, "", pkg)
    total[pkg] += stmts[key]; if (key in hit) covered[pkg] += stmts[key]
  }
  n = asorti(total, pkgs)
  if (md == "--markdown") { print "| package | coverage |"; print "|---|---:|" }
  for (i = 1; i <= n; i++) {
    p = pkgs[i]; pct = 100 * covered[p] / total[p]
    allT += total[p]; allC += covered[p]
    if (md == "--markdown") printf "| `%s` | %.1f%% |\n", p, pct; else printf "%-32s %6.1f%%\n", p, pct
  }
  pct = allT ? 100 * allC / allT : 0
  if (md == "--markdown") printf "| **total** | **%.1f%%** |\n", pct; else printf "%-32s %6.1f%%\n", "total", pct
}' "$profile"
