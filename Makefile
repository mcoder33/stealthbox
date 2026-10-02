.PHONY: build test e2e e2e-workspaces release windows-payload
VERSION ?= dev
build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/stealthbox ./cmd/stealthbox
test:
	go test -race ./...
	go vet ./...
e2e:
	python3 tests/integration/e2e.py
	python3 tests/integration/workspaces.py
e2e-workspaces:
	python3 tests/integration/workspaces.py
windows-payload:
	mkdir -p cmd/stealthbox/_payload
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=$(VERSION)" -o cmd/stealthbox/_payload/linux-amd64 ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=$(VERSION)" -o cmd/stealthbox/_payload/linux-arm64 ./cmd/stealthbox
release: windows-payload
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-darwin-arm64 ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-darwin-amd64 ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-linux-arm64 ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-linux-amd64 ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-windows-amd64.exe ./cmd/stealthbox
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags "-X main.version=$(VERSION)" -o dist/stealthbox-windows-arm64.exe ./cmd/stealthbox
	cd dist && shasum -a 256 stealthbox-* > checksums.txt
