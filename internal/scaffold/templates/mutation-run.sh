#!/bin/sh
set -eu

kit_module=github.com/raulfrk/herdr-plugin-kit
kit_dir=$(GOWORK=off go list -mod=readonly -m -f '{{if .Replace}}HERDR_REPLACED{{else}}{{.Dir}}{{end}}' "$kit_module")
if [ "$kit_dir" = HERDR_REPLACED ]; then
  echo "mutation testing requires the pinned Herdr Plugin Kit module without replace" >&2
  exit 2
fi
exec sh "$kit_dir/scripts/mutation-run" "$@"
