# Validation workflow

Run these commands from the generated plugin directory, substituting its stable
plugin ID in the build path:

```sh
# Run this once for a newly generated plugin. Preserve an existing plugin's
# dependency metadata during its pre-change validation.
go mod tidy

go run github.com/raulfrk/herdr-plugin-kit/cmd/herdr-plugin-kit@v0.2.0 validate .
go test ./...
go test -race ./...
go build -mod=mod -buildvcs=false -o ./plugin ./cmd/<plugin-id>
```

Use the version already pinned by an existing plugin unless the user requests an
upgrade. The generator emits a minimal `go.mod`; the one-time `go mod tidy`
creates the standalone dependency metadata needed by a new plugin's test
commands. Do not run it before baseline validation of an existing plugin. Run
any additional repository checks after these commands.

Validation proves the static scaffold contract, tests, concurrency checks, and
build. It does not prove behavior inside Herdr. When the target host is
available, use the plugin repository's documented installation procedure and
verify the user-facing action or pane at the required viewport. Record what was
observed and distinguish failures of diagnostic collection from failures of the
plugin behavior itself.

If the target host, credentials, service, or physical viewport is unavailable,
state the exact unverified assumption. Do not fabricate a live result or infer
one from unit tests.
