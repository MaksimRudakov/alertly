SHELL := /usr/bin/env bash

BINARY      := alertly
PKG         := github.com/MaksimRudakov/alertly
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
               -X $(PKG)/internal/version.Version=$(VERSION) \
               -X $(PKG)/internal/version.Commit=$(COMMIT) \
               -X $(PKG)/internal/version.Date=$(DATE)

GO          ?= go
GOFLAGS     ?=
DOCKER_TAG  ?= alertly:dev

.PHONY: all build test test-cover lint fmt run docker clean tidy

all: lint test build

build:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/alertly

test:
	$(GO) test -race -count=1 ./...

test-cover:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1
	$(GO) tool cover -html=coverage.out -o coverage.html

lint:
	@if command -v golangci-lint >/dev/null; then \
	  golangci-lint run; \
	else \
	  echo "golangci-lint not installed; running go vet + staticcheck fallback"; \
	  $(GO) vet ./...; \
	  command -v staticcheck >/dev/null && staticcheck ./... || true; \
	fi

fmt:
	gofmt -w .
	@command -v goimports >/dev/null && goimports -w . || true

run:
	ALERTLY_CONFIG=examples/config.yaml $(GO) run ./cmd/alertly

docker:
	docker build -t $(DOCKER_TAG) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin dist coverage.out coverage.html
