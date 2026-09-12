VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X github.com/plytz/caramelo/internal/cli.version=$(VERSION)
BIN      = bin/caramelo

SUITE ?=
ARGS  ?=

E2E_SUITES = machine setup bootstrap api env run vpn edge prod reboot fleet

CARAMELO_ITEST_FACTOR ?= 1.5

ifeq ($(strip $(SUITE)),)
INTEGRATION_PKGS = ./test/integration/...
E2E_PKGS         = $(patsubst %,./test/integration/%/...,$(E2E_SUITES))
else
INTEGRATION_PKGS = ./test/integration/$(SUITE)/...
E2E_PKGS         = ./test/integration/$(SUITE)/...
endif

.PHONY: all build check integration integration-clean e2e manual clean

all: check build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/caramelo

check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	CGO_ENABLED=0 go build -trimpath ./...
	go vet ./...
	go vet -tags integration ./...
	go vet -tags integration,e2e ./...
	go test -race -count=1 ./...

integration:
	go test -tags integration -count=1 -p 1 -v -timeout 120m $(ARGS) $(INTEGRATION_PKGS)

integration-clean:
	@ids=$$(docker ps -aq --filter label=caramelo.itest=1); \
	if [ -n "$$ids" ]; then docker rm -f -v $$ids; fi
	@nets=$$(docker network ls -q --filter label=caramelo.itest=1); \
	if [ -n "$$nets" ]; then docker network rm $$nets; fi
	@vols=$$(docker volume ls -q --filter label=caramelo.itest=1); \
	if [ -n "$$vols" ]; then docker volume rm $$vols; fi

e2e:
	@test -n "$(INVENTORY)" || { echo 'usage: make e2e INVENTORY=<inventory.json> [SUITE=<name>] [ARGS=...]; a clean machine between suites is the caller job unless CARAMELO_E2E_RESET names a command'; exit 1; }
	CARAMELO_E2E_INVENTORY=$(abspath $(INVENTORY)) CARAMELO_ITEST_FACTOR=$(CARAMELO_ITEST_FACTOR) go test -tags integration,e2e -count=1 -p 1 -v -timeout 180m $(ARGS) ./test/e2e/preflight/... ./test/e2e/sshrun/...
	CARAMELO_E2E_INVENTORY=$(abspath $(INVENTORY)) CARAMELO_ITEST_FACTOR=$(CARAMELO_ITEST_FACTOR) go test -tags integration,e2e -count=1 -p 1 -v -timeout 180m $(ARGS) $(E2E_PKGS)

manual:
	go run ./cmd/caramelo manual --markdown > MANUAL.md

clean:
	rm -rf bin
