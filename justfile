# Unit tests and vet. Runs on any platform.
test:
    go vet ./...
    go test -race ./...

# CI runs this recipe. Arguments are `go test` flags and packages: `just test-integration -v ./internal/tunnel`.
# Linux-only tests: policy routing, nftables, end-to-end tunnels. Runs in Docker so it works from macOS.
test-integration *args='./...':
    docker build -q -f deploy/Dockerfile.test -t lotsman-test . >/dev/null
    docker run --rm --privileged lotsman-test {{args}}

# Formatting, vet, module integrity, staticcheck and govulncheck for the host
# and the Linux integration build, and the generated API code. CI runs this too.
lint: gen-check
    test -z "$(gofmt -l .)"
    go vet ./...
    GOOS=linux go vet -tags integration ./...
    go mod verify
    go tool staticcheck ./...
    # `go tool` would build staticcheck for GOOS=linux and fail to exec it, so build it for the host first.
    GOOS=linux "$(go tool -n staticcheck)" -tags integration ./...
    go tool govulncheck ./...
    GOOS=linux "$(go tool -n govulncheck)" -tags integration ./...

# Static Linux binary. `just build arm64` for another architecture.
build arch="amd64":
    CGO_ENABLED=0 GOOS=linux GOARCH={{arch}} go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty)" -o dist/lotsman-linux-{{arch}} ./cmd/lotsman

# Regenerates the control API's Go code from api/. buf is pinned in tools/go.mod,
# the plugins in go.mod at the runtime's versions, so nothing is installed globally.
gen:
    find api \( -name '*.pb.go' -o -name '*.connect.go' \) -delete
    go tool -modfile=tools/go.mod buf generate

# Fails when the protos are unformatted or break the lint rules, or when the
# generated code differs from what `just gen` would write.
gen-check:
    #!/bin/sh
    set -eu
    buf() { go tool -modfile=tools/go.mod buf "$@"; }
    buf format --diff --exit-code
    buf lint
    out=$(mktemp -d)
    trap 'rm -rf "$out"' EXIT
    buf generate -o "$out"
    diff -r -x '*.proto' "$out/api" api || { echo "generated code is stale: run just gen" >&2; exit 1; }
