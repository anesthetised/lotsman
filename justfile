# Unit tests and vet. Runs on any platform.
test:
    go vet ./...
    go test -race ./...

# Linux-only tests: policy routing, nftables, end-to-end tunnels. Runs in Docker so it works from macOS.
test-integration:
    docker build -q -f deploy/Dockerfile.test -t lotsman-test . >/dev/null
    docker run --rm --privileged lotsman-test

# Formatting and cross-platform vet, including the Linux integration build.
lint:
    gofmt -l .
    GOOS=linux go vet -tags integration ./...

# Static Linux binary. `just build arm64` for another architecture.
build arch="amd64":
    CGO_ENABLED=0 GOOS=linux GOARCH={{arch}} go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty)" -o dist/lotsman-linux-{{arch}} ./cmd/lotsman
