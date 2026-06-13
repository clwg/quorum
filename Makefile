.PHONY: gen build install clean test vet tidy certs run-server run-client run-admin run-gui $(CMDS)

GO      ?= go
BIN     ?= bin
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w

# Buildable commands under ./cmd; each becomes ./$(BIN)/<name>.
CMDS := quorum-server quorum-client quorum-admin quorum-gui quorum-gencert

gen:
	buf lint
	buf generate

# Compile every command into ./$(BIN).
build: $(CMDS)

# Build a single command, e.g. `make quorum-server`.
$(CMDS):
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/$@ ./cmd/$@

clean:
	rm -rf $(BIN)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

certs:
	$(GO) run ./cmd/quorum-gencert -out certs

run-server:
	$(GO) run ./cmd/quorum-server --listen :8443 --cert certs/server.pem --key certs/server-key.pem --db quorum.db

run-client:
	$(GO) run ./cmd/quorum-client --addr localhost:8443 --ca certs/ca.pem

run-admin:
	$(GO) run ./cmd/quorum-admin --addr localhost:8443 --ca certs/ca.pem

run-gui:
	$(GO) run ./cmd/quorum-gui --addr localhost:8443 --ca certs/ca.pem
