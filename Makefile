.PHONY: build test run migrate mocks lint clean

BIN_DIR := bin
SERVER_BIN := $(BIN_DIR)/server

build:
	go build -o $(SERVER_BIN) ./cmd/server

test:
	go test ./...

run: build
	$(SERVER_BIN)

migrate:
	go run ./cmd/server -migrate-only

mocks:
	go generate ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BIN_DIR) data logs *.db *.db-shm *.db-wal
