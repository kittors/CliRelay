#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

./scripts/ensure-no-vendored-panel-assets.sh
python3 scripts/check-backend-structure.py

files="$(gofmt -l . || true)"
if [ -n "${files}" ]; then
  echo "gofmt needed for:"
  echo "${files}"
  exit 1
fi

python3 - <<'PY'
import os
import re
import sys

key_re = re.compile(r"\bsk-[A-Za-z0-9]{16,}\b")
skip_dirs = {".git", ".gocache", ".tmp-go", ".agentflow"}
skip_ext = {".png", ".jpg", ".jpeg", ".gif", ".ico", ".svg", ".pdf", ".zip"}

bad = []
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in skip_dirs]
    for fn in files:
        _, ext = os.path.splitext(fn)
        if ext.lower() in skip_ext:
            continue
        path = os.path.join(root, fn)
        try:
            with open(path, "rb") as f:
                data = f.read()
        except OSError:
            continue

        if b"\x00" in data:
            continue
        text = data.decode("utf-8", errors="ignore")

        for match in key_re.finditer(text):
            key = match.group(0)
            if ("X" in key) or ("*" in key):
                continue
            bad.append((path, key))

if bad:
    print("Found real-looking API key(s). Use placeholders like 'sk-...XXXX...' or masked forms.")
    for path, key in bad[:50]:
        print(f"  {path}: {key}")
    sys.exit(1)
PY

go vet ./...
go test ./...

required_golangci_lint_version="v1.64.5"
golangci_lint_bin="${GOLANGCI_LINT_BIN:-}"
if [ -z "${golangci_lint_bin}" ]; then
  if command -v golangci-lint >/dev/null 2>&1; then
    golangci_lint_bin="$(command -v golangci-lint)"
  elif [ -x "$(go env GOPATH)/bin/golangci-lint" ]; then
    golangci_lint_bin="$(go env GOPATH)/bin/golangci-lint"
  else
    echo "golangci-lint ${required_golangci_lint_version} is required." >&2
    echo "Install it once with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@${required_golangci_lint_version}" >&2
    exit 1
  fi
fi

if [ ! -x "${golangci_lint_bin}" ]; then
  echo "golangci-lint is not executable: ${golangci_lint_bin}" >&2
  exit 1
fi

golangci_lint_version="$("${golangci_lint_bin}" version)"
if ! grep -Eq 'version v?1\.64\.5([[:space:]]|$)' <<<"${golangci_lint_version}"; then
  echo "golangci-lint ${required_golangci_lint_version} is required; found: ${golangci_lint_version}" >&2
  exit 1
fi

"${golangci_lint_bin}" run --config .golangci.yml

trap 'rm -f test-output' EXIT
go build -o test-output ./cmd/server
