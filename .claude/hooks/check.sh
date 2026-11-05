#!/usr/bin/env bash
# Stop: vet and test before Claude finishes. Exit 2 makes Claude keep working on failures.
if ! out=$(go vet ./... 2>&1 && go test -race ./... 2>&1); then
  echo "go vet or tests are failing. Fix them before finishing:" >&2
  echo "$out" | tail -40 >&2
  exit 2
fi
exit 0
