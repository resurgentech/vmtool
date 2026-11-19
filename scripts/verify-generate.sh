#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go generate ./internal/api/... ./internal/storeapi/...
if ! git diff --exit-code -- internal/api internal/storeapi; then
  echo "ERROR: generated code is stale. Run: go generate ./internal/api/... ./internal/storeapi/..."
  exit 1
fi
