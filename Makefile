.PHONY: build test vet lint bench e2e docker
build:
	go build ./...
vet:
	go vet ./...
test:
	go test -race ./...
lint:
	golangci-lint run
bench:
	go run ./cmd/bench -ops 1000
e2e:
	./hack/e2e-kind.sh
docker:
	docker build -t ghcr.io/ayushgawai/snapvol-csi:latest .
