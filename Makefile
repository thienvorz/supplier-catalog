.PHONY: build test race vet fmt check supplier server demo clean

build:
	go build -o bin/server ./cmd/server
	go build -o bin/fakesupplier ./cmd/fakesupplier

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

# everything that should be green before publishing
check: fmt vet race

supplier:
	go run ./cmd/fakesupplier

server:
	go run ./cmd/server

demo:
	./scripts/demo.sh

clean:
	rm -rf bin catalog.db catalog.db-wal catalog.db-shm
