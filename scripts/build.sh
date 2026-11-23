#!/usr/bin/env bash
# Builds the plugin for both architectures and writes its manifest:
#   scripts/build.sh VERSION [OUTPUT_DIR]
# The output directory then holds plugin-amd64.tar.gz, plugin-arm64.tar.gz
# and plugin.json. Signing is a separate step (the release workflow, or
# `go run github.com/agim/lidza-deploy/cmd/plugin-sign sign`).
set -euo pipefail
cd "$(dirname "$0")/.."
version=${1:?version required, for example 0.1}
output=${2:-dist}
[[ "$version" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]] || { echo 'the version must look like 1, 1.2 or 1.2.3' >&2; exit 2; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$output"
for arch in amd64 arm64; do
  mkdir "$work/$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o "$work/$arch/plugin" .
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='UTC 2020-01-01' -C "$work/$arch" -czf "$output/plugin-$arch.tar.gz" plugin
done
go run github.com/agim/lidza-deploy/cmd/plugin-sign manifest -name clickhouse -version "$version" -provides clickhouse \
  -summary 'Runs the ClickHouse server of a host for the applications on it.' \
  -hooks describe,check,status,repair,provision,release,backup,restore,maintain,export,import \
  -env CLICKHOUSE_URL,CLICKHOUSE_DATABASE,CLICKHOUSE_USER,CLICKHOUSE_PASSWORD \
  -archive "amd64=$output/plugin-amd64.tar.gz" -archive "arm64=$output/plugin-arm64.tar.gz" -out "$output/plugin.json"
echo "Built $output/plugin.json and the archives for version $version."
