# Local gates for ZSoftly Core. `make check` is the merge gate; there is no
# hosted CI workflow. See docs/development.md.

GO        ?= go
BIN       := bin/zs-core
GEN_OUT   := internal/httpapi/gen/types.gen.go
GEN_CFG   := api/oapi-codegen.yaml
SPEC      := api/openapi.yaml
GO_FILES   = $(shell find . -name '*.go' -not -path './vendor/*' -not -path './bin/*')

COMPOSE_DIR := deploy/compose
COMPOSE     := docker compose --project-directory $(COMPOSE_DIR) -f $(COMPOSE_DIR)/compose.yaml

.PHONY: fmt fmt-check vet lint test test-race test-integration build generate generate-check check clean \
	stack-up stack-down stack-reset stack-logs stack-bootstrap stack-apply stack-verify doctor stack-guard

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

lint: vet
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./... && staticcheck -tags integration ./...; \
	else \
		echo "staticcheck not installed: lint ran go vet only"; \
	fi

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

# Runs unit and integration tests. Integration tests use ZS_TEST_POSTGRES_URL
# when set, otherwise a Postgres 18 container through Docker, and skip with a
# message when neither is available. Without ZS_TEST_POSTGRES_URL they start
# containers, so they run only on a build host: one that sets ZS_BUILD_HOST=1.
test-integration:
	@if [ "$${ZS_BUILD_HOST:-}" != "1" ] && [ -z "$${ZS_TEST_POSTGRES_URL:-}" ]; then \
		echo "refusing to run the integration tests: set ZS_BUILD_HOST=1 on a build host, or ZS_TEST_POSTGRES_URL to a Postgres server (docs/development.md)"; exit 1; \
	fi
	$(GO) test -tags integration -count=1 ./...

build:
	$(GO) build -trimpath -o $(BIN) ./cmd/zs-core

generate:
	$(GO) generate ./...

generate-check:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(GO) tool oapi-codegen -config $(GEN_CFG) -o "$$tmp/types.gen.go" $(SPEC) && \
	if ! cmp -s "$$tmp/types.gen.go" $(GEN_OUT); then \
		echo "$(GEN_OUT) is stale: run make generate"; exit 1; \
	fi

check: fmt-check vet test generate-check

clean:
	rm -rf bin

# Reference stack. These targets run docker compose in the current shell, on the
# host that will run the containers. That host is a build VM, never a
# workstation: see deploy/compose/README.md. They run only where ZS_BUILD_HOST=1
# is set, which the build VM does in /etc/environment.
stack-guard:
	@if [ "$${ZS_BUILD_HOST:-}" != "1" ]; then \
		echo "refusing to run the reference stack: ZS_BUILD_HOST=1 is not set. Run it on the build VM (deploy/compose/README.md)"; exit 1; \
	fi

# Builds the Keycloak and Core images, starts everything and waits for health.
stack-up: stack-guard
	@test -f $(COMPOSE_DIR)/.env || cp $(COMPOSE_DIR)/.env.example $(COMPOSE_DIR)/.env
	$(COMPOSE) up -d --build --wait

stack-down: stack-guard
	$(COMPOSE) down

# Stops the stack and deletes its volumes: every database is wiped.
stack-reset: stack-guard
	$(COMPOSE) down --volumes --remove-orphans

stack-logs: stack-guard
	$(COMPOSE) logs -f --tail=100

# Creates the zs-reference Kill Bill tenant and uploads the test catalogue.
stack-bootstrap: stack-guard
	$(COMPOSE_DIR)/killbill/bootstrap.sh

# Runs zs-core doctor inside the running core container.
doctor: stack-guard
	$(COMPOSE) exec core zs-core doctor

# Applies brands/zs-reference.yaml with zs-core brand apply in the core
# container.
stack-apply: stack-guard
	$(COMPOSE) exec -T core zs-core brand apply --operator stack-apply --reason 'reference stack' brands/zs-reference.yaml > /dev/null

# The configuration-as-code check: from a clean stack (make stack-reset
# first), these steps alone must reach a green doctor. No console action and
# no manual database change is part of it.
stack-verify: stack-guard
	$(MAKE) stack-up
	$(MAKE) stack-bootstrap
	$(MAKE) stack-apply
	$(COMPOSE) exec -T core zs-core doctor
