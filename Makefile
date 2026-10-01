.PHONY: build test e2e release
VERSION ?= dev
build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/stealthbox ./cmd/stealthbox
test:
	go test -race ./...
	go vet ./...
e2e:
	python3 tests/integration/e2e.py
release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-darwin-arm64 ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-darwin-amd64 ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-linux-arm64 ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-linux-amd64 ./cmd/stealthbox
	cd dist && shasum -a 256 stealthbox-* > checksums.txt
