#!/usr/bin/env bash
# Cumulative coverage across both Go modules and every Python suite.
#
# Each run adds raw Go coverage to one directory, so unit tests, envtests and
# instrumented harness binaries (GOCOVERDIR, see harness/e2e/runner/README.md)
# merge into one profile. Functions that stay at 0% across all of them are
# candidates for dead code.
#
#   scripts/coverage.sh            run every suite, then report
#   scripts/coverage.sh report     report only (after adding GOCOVERDIR data)
#   COVERAGE_RESET=1 scripts/coverage.sh   start from an empty profile
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
out=${COVERAGE_DIR:-$root/.coverage-data}
go_raw=$out/go
py_data=$out/python

if [[ "${COVERAGE_RESET:-}" == 1 ]]; then
  rm -rf "${out:?}"
fi
mkdir -p "$go_raw" "$py_data"

# Every suite runs even after a failure, so a failing package cannot
# silently shrink the profile; the exit status still reports it.
failed=()
if [[ "${1:-}" != report ]]; then
  for module in controller harness/e2e; do
    (cd "$root/$module" && GOWORK=off GOMAXPROCS=${GOMAXPROCS:-4} \
      go test -count=1 -p 2 -cover -coverpkg=./... ./... -args -test.gocoverdir="$go_raw") || failed+=("$module")
  done
  for suite in harness/e2e/aws harness/e2e/observe harness/e2e/runner images providers/k0s .github; do
    (cd "$root/$suite" && COVERAGE_FILE="$py_data/.coverage.${suite//\//_}" \
      python3 -m coverage run --branch --source=. -m unittest discover -s . -p 'test_*.py') || failed+=("$suite")
  done
fi

# Raw data from every run merges by package path, across both modules and
# any instrumented binary; covdata reads it without the sources.
go tool covdata textfmt -i="$go_raw" -o="$out/go.txt"
go tool covdata percent -i="$go_raw" > "$out/go-packages.txt"
go tool covdata func -i="$go_raw" > "$out/go-func.txt"
tail -n 1 "$out/go-func.txt"
awk '$NF == "0.0%" {print $1, $2}' "$out/go-func.txt" > "$out/go-uncovered.txt"
echo "Go functions never executed: $(wc -l < "$out/go-uncovered.txt") (see $out/go-uncovered.txt)"

(cd "$py_data" && python3 -m coverage combine --keep -q .coverage.* &&
  python3 -m coverage report --skip-covered --sort=cover > "$out/python.txt" || true)
tail -n 1 "$out/python.txt"
echo "Python per-file report: $out/python.txt"
if ((${#failed[@]})); then
  echo "failing suites: ${failed[*]}" >&2
  exit 1
fi
