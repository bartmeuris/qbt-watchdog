VERSION ?= $(shell version=$$(git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null); test -n "$$version" && printf '%s' "$$version" | cut -c2- || printf 'dev')
REVISION ?= $(shell git rev-parse --verify 'HEAD^{commit}' 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION) -X main.revision=$(REVISION) -X main.buildDate=$(BUILD_DATE)

.PHONY: fmt test race vet build docker-build
fmt:
	gofmt -w .
test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...
build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="$(LDFLAGS)" ./cmd/qbt-watchdog
docker-build:
	docker build -t qbt-watchdog:test \
		--build-arg VERSION=$(VERSION) \
		--build-arg REVISION=$(REVISION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .
