#!/usr/bin/env bash
# Run the wire-protocol driver compatibility suite (spec 2061 doc 19 appendix G).
#
# Builds the doc binary once, then drives each MongoDB driver against a fresh `doc serve`
# instance. Every arm boots its own server on an ephemeral loopback port, runs its suite, and
# shuts the server down. A driver whose toolchain is not installed is skipped with a notice
# rather than failing the run, so a developer with only Go installed still gets the Go arm.
#
# Exit status is non-zero if any arm that ran reported a failure.
set -u

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="$(mktemp -d)/doc"
echo "building doc binary"
go build -o "$bin" github.com/tamnd/doc/cmd/doc || { echo "build failed"; exit 1; }
export DOC_BIN="$bin"

rc=0
ran=0

run_arm() {
  local name="$1"; shift
  echo
  echo "=== $name ==="
  ran=$((ran + 1))
  if "$@"; then
    echo "$name: ok"
  else
    echo "$name: FAILED"
    rc=1
  fi
}

# Go: the primary driver, full CRUD, aggregation, transactions, indexes.
if command -v go >/dev/null 2>&1; then
  run_arm "go driver" bash -c "cd '$root/compat/go' && go test ./... -count=1"
else
  echo "skip go arm: go not found"
fi

# Python: full CRUD, aggregation, transactions.
py=""
for cand in "$root/compat/python/.venv/bin/python" python3 python; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c "import pymongo" >/dev/null 2>&1; then
    py="$cand"; break
  fi
done
if [ -n "$py" ]; then
  run_arm "python driver" "$py" "$root/compat/python/run_compat.py"
else
  echo "skip python arm: a python with pymongo was not found (pip install pymongo)"
fi

# Node: full CRUD, aggregation.
if command -v node >/dev/null 2>&1 && [ -d "$root/compat/node/node_modules/mongodb" ]; then
  run_arm "node driver" node "$root/compat/node/run_compat.mjs"
else
  echo "skip node arm: node with the mongodb package was not found (cd compat/node && npm install)"
fi

echo
if [ "$ran" -eq 0 ]; then
  echo "no driver toolchains available; nothing ran"
  exit 1
fi
if [ "$rc" -eq 0 ]; then
  echo "all $ran driver arm(s) passed"
else
  echo "one or more driver arms failed"
fi
exit "$rc"
