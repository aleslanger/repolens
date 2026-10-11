# Development helpers. `make dev` rebuilds and restarts the server in the Herdr "server" pane.
BIN := bin/repolens
PORT ?= 8765
SERVER_PANE ?= $(shell herdr pane list --workspace "$$HERDR_WORKSPACE_ID" 2>/dev/null | grep -o '"label":"server","pane_id":"[^"]*"' | sed 's/.*"pane_id":"\([^"]*\)"/\1/')

.PHONY: build test cover vet fmt serve dev export collect

build:
	go build -o $(BIN) ./cmd/repolens

test:
	go test -race ./...

cover:
	go test -cover ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }

serve: build
	./$(BIN) serve --port $(PORT)

dev: build
	@test -n "$(SERVER_PANE)" || { echo "no Herdr pane labelled 'server'"; exit 1; }
	herdr pane send-keys $(SERVER_PANE) ctrl+c >/dev/null
	herdr pane run $(SERVER_PANE) "clear" >/dev/null
	herdr pane wait-output $(SERVER_PANE) --source visible --regex '^[^b]*$$' --timeout 5000 >/dev/null
	herdr pane run $(SERVER_PANE) "./$(BIN) serve --port $(PORT)" >/dev/null
	@for i in $$(seq 50); do curl -sf -o /dev/null http://127.0.0.1:$(PORT)/api/status && { echo "server restarted on :$(PORT)"; exit 0; }; sleep 0.2; done; echo "server did not start"; exit 1

collect: build
	./$(BIN) collect --dir ~/projects

export: build
	./$(BIN) export --out repolens.html
