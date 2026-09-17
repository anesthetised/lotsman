.PHONY: test test-integration lint

test:
	go vet ./...
	go test -race ./...

# Linux-only tests: policy routing, nftables, end-to-end tunnels. Runs in Docker so it works from macOS.
test-integration:
	docker build -q -f deploy/Dockerfile.test -t lotsman-test . >/dev/null
	docker run --rm --privileged lotsman-test

lint:
	gofmt -l . && GOOS=linux go vet -tags integration ./...
