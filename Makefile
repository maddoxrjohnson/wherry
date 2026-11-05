# Go computes the dependency graph itself; this file only adds flags and names.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build dist run test lint cover clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/wherryd ./cmd/wherryd
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/wherry ./cmd/wherry

# One tarball per architecture with both binaries, the units, and the scripts.
dist:
	rm -rf dist
	set -e; for arch in amd64 arm64; do \
		dir=dist/wherry_$(VERSION)_linux_$$arch; mkdir -p $$dir; \
		for cmd in wherryd wherry; do \
			CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/$$cmd ./cmd/$$cmd; \
		done; \
		cp LICENSE deploy/systemd/wherryd.service deploy/systemd/wherry.env.example $$dir/; \
		cp deploy/systemd/wherry-backup.service deploy/systemd/wherry-backup.timer deploy/scripts/* $$dir/; \
		tar --owner=0 --group=0 --numeric-owner -czf $$dir.tar.gz -C dist $$(basename $$dir); \
		rm -rf $$dir; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS

# Text logs read better in a terminal; production keeps the JSON default.
run: build
	WHERRY_LOG_FORMAT=text ./bin/wherryd

test:
	go test -race -count=1 ./...

lint:
	go vet ./...
	golangci-lint run ./...

cover:
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

clean:
	rm -rf bin dist coverage.out
