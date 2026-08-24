.DEFAULT_GOAL := help

.PHONY: help check check-router check-crw test test-router test-crw test-appliance \
	router-fmt-check router-vet router-race compose-config check-stack-lock build-router build-crw-image \
	build-images up down ps logs pull smoke live-contract backup restore check-updates

GO_IMAGE = golang:1.24.6-bookworm@sha256:ab1d1823abb55a9504d2e3e003b75b36dbeb1cbcc4c92593d85a84ee46becc6c
GO_DOCKER = docker run --rm -v "$(CURDIR)/router:/src" -w /src $(GO_IMAGE)
COMPOSE = docker compose --project-directory deploy -f deploy/compose.yaml
ROUTER_IMAGE ?= hermes-web-retrieval-router:dev
CRW_BUILD_IMAGE ?= hermes-web-retrieval-crw:dev
# The combined monorepo gate favors bounded artifacts over debugger symbols.
# Component developers can still run the native crw/Makefile directly.
CRW_CARGO_ENV = CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=0 CARGO_PROFILE_TEST_DEBUG=0

help:
	@printf '%s\n' \
	  'Verification:' \
	  '  make check             Run all component and appliance checks' \
	  '  make test              Run all component and appliance tests' \
	  '  make check-router      Format, vet, test, and race-check the Go router' \
	  '  make check-crw         Run the CRW workspace checks' \
	  '  make test-appliance    Run Compose and backup/restore contracts' \
	  '' \
	  'Build and operations:' \
	  '  make build-images      Build local router and CRW images' \
	  '  make up|down|ps        Manage the appliance in deploy/' \
	  '  make smoke             Run the bounded production smoke test' \
	  '  make live-contract     Run search, scrape, cache, and concurrency gates'

check: check-router check-crw test-appliance compose-config check-stack-lock

check-router: router-fmt-check router-vet test-router router-race

check-crw:
	$(CRW_CARGO_ENV) $(MAKE) -C crw check

test: test-router test-crw test-appliance

test-router:
	$(GO_DOCKER) go test ./...

test-crw:
	$(CRW_CARGO_ENV) $(MAKE) -C crw test

test-appliance:
	python3 -m unittest discover -s tests/appliance -p '*_test.py' -v

router-fmt-check:
	$(GO_DOCKER) sh -c 'test -z "$$(gofmt -l .)"'

router-vet:
	$(GO_DOCKER) go vet ./...

router-race:
	$(GO_DOCKER) go test -race ./...

compose-config:
	$(COMPOSE) config --quiet

check-stack-lock:
	python3 scripts/check_stack_lock.py

build-router:
	docker build --pull -t $(ROUTER_IMAGE) router

build-crw-image:
	docker build --pull -t $(CRW_BUILD_IMAGE) crw

build-images: build-router build-crw-image

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

ps:
	$(COMPOSE) ps

logs:
	$(COMPOSE) logs -f $(SERVICES)

pull:
	$(COMPOSE) pull

smoke:
	./scripts/smoke-test.sh

live-contract:
	./scripts/live-contract-test.py

backup:
	./scripts/backup.sh "$(BACKUP)"

restore:
	./scripts/restore.sh --force "$(BACKUP)"

check-updates:
	./scripts/check-updates.sh
