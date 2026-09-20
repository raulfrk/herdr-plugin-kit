#!/bin/sh
set -eu

kit_module=github.com/raulfrk/herdr-plugin-kit
kit_selection=$(GOWORK=off go list -mod=readonly -m -f '{{if .Replace}}HERDR_REPLACED{{else}}{{.Path}}@{{.Version}}{{end}}' "$kit_module")
if [ "$kit_selection" = HERDR_REPLACED ]; then
  echo "mutation testing requires the pinned Herdr Plugin Kit module without replace" >&2
  exit 2
fi
case "$kit_selection" in
  "$kit_module"@v*) ;;
  *) echo "cannot resolve a pinned Herdr Plugin Kit version" >&2; exit 2 ;;
esac
GOWORK=off go mod download "$kit_selection"
kit_dir=$(GOWORK=off go list -mod=readonly -m -f '{{.Dir}}' "$kit_module")
if [ -z "$kit_dir" ] || [ ! -d "$kit_dir" ]; then
  echo "cannot resolve the Herdr Plugin Kit module directory" >&2
  exit 2
fi
exec sh "$kit_dir/scripts/mutation-run" "$@"
