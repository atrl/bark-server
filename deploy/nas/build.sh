#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
output_dir="${1:-$repo_root/dist/nas}"
if [[ -n "$(git -C "$repo_root" status --porcelain)" ]]; then
  printf 'Commit or preserve pending changes before building a release.\n' >&2
  exit 1
fi
revision="$(git -C "$repo_root" rev-parse HEAD)"
build_date="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

mkdir -p "$output_dir"
cd "$repo_root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=nas-fcm -X main.commitID=$revision -X main.buildDate=$build_date" \
  -o "$output_dir/bark-server" .
cp deploy/nas/Dockerfile "$output_dir/Dockerfile"
printf '%s\n' "$revision" > "$output_dir/REVISION"
printf 'Build context: %s\nRevision: %s\n' "$output_dir" "$revision"
