#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$ROOT"

if [ ! -d "node_modules" ]; then
  pnpm install --frozen-lockfile 2>/dev/null || pnpm install
fi

pnpm typecheck
pnpm test:run
