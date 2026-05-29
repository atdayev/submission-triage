.PHONY: build test test-e2e run demo migrate mocks lint clean

BIN_DIR := bin
SERVER_BIN := $(BIN_DIR)/server
REPLAY_BIN := $(BIN_DIR)/replay

build:
	go build -o $(SERVER_BIN) ./cmd/server
	go build -o $(REPLAY_BIN) ./cmd/replay

test:
	go test -short ./...

test-e2e:
	go test -tags=e2e ./test/e2e/...

run: build
	$(SERVER_BIN)

demo: build
	@mkdir -p data logs
	$(SERVER_BIN) & echo $$! > .server.pid
	@sleep 2
	$(REPLAY_BIN) -dir ./testdata/eml -url http://localhost:8080/webhooks/postmark
	@tail -f logs/submission-triage.log || true
	@kill `cat .server.pid` 2>/dev/null || true
	@rm -f .server.pid

migrate:
	go run ./cmd/server -migrate-only

mocks:
	go generate ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BIN_DIR) data logs *.db *.db-shm *.db-wal coverage.out
