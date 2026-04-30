.PHONY: build test lint

build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/bolthub-finalize-daemon-linux-amd64 ./cmd/finalize-daemon
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/bolthub-finalize-daemon-linux-arm64 ./cmd/finalize-daemon

test:
	go test ./...

lint:
	go vet ./...
