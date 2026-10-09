.PHONY: fmt test race vet build container-binaries docker-build
fmt:
	gofmt -w .
test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...
build:
	CGO_ENABLED=0 go build -trimpath ./cmd/qbt-watchdog
container-binaries:
	mkdir -p dist/linux/amd64 dist/linux/arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/linux/amd64/qbt-watchdog ./cmd/qbt-watchdog
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/linux/arm64/qbt-watchdog ./cmd/qbt-watchdog
docker-build: container-binaries
	docker build -t qbt-watchdog:test .
