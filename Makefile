.PHONY: build test test-integration test-stress test-all run migrate mocks lint clean deploy

BIN_DIR := bin
SERVER_BIN := $(BIN_DIR)/server

build:
	go build -o $(SERVER_BIN) ./cmd/server

test:
	go test ./...

# in-memory IMAP/SMTP servers; no network or credentials needed
test-integration:
	go test -tags integration ./...

# real SQLite + real concurrency; slower, and the only suite that drives the reply
# pool against the outbox sweeper
test-stress:
	go test -tags stress -timeout 15m ./internal/service/

test-all: test test-integration test-stress

run: build
	$(SERVER_BIN)

migrate:
	go run ./cmd/server -migrate-only

mocks:
	go generate ./...

lint:
	golangci-lint run ./...

# removes build output and local run state (database, logs) — all gitignored.
# the bare binary names are what `go build` drops in the repo root when it is run
# without -o; they are easy to leave behind because nothing else references them.
clean:
	rm -rf $(BIN_DIR) data logs *.db *.db-shm *.db-wal
	rm -f submission-triage submission-triage.exe server server.exe

# cross-compile and ship to the host configured in .deploy.env (gitignored)
deploy:
	bash scripts/deploy.sh
