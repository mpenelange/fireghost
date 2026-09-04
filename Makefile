.DEFAULT_GOAL := help

.PHONY: help check check-router check-crw test test-router test-crw test-appliance \
	test-hermes-regression hermes-regression \
	router-fmt-check router-vet router-race compose-config check-stack-lock build-router build-crw-image \
	build-images up down ps logs pull smoke live-contract staging-up staging-down staging-ps \
	staging-smoke staging-live-contract backup restore check-updates

GO_IMAGE = golang:1.24.6-bookworm@sha256:ab1d1823abb55a9504d2e3e003b75b36dbeb1cbcc4c92593d85a84ee46becc6c
GO_DOCKER = docker run --rm -v "$(CURDIR)/router:/src" -w /src $(GO_IMAGE)
COMPOSE = docker compose --project-directory deploy -f deploy/compose.yaml
STAGING_PROJECT ?= hermes-web-retrieval-staging
STAGING_ROUTER_VERSION ?= monorepo-staging
STAGING_COMPOSE = $(COMPOSE) -f deploy/compose.staging.yaml -p $(STAGING_PROJECT)
ROUTER_IMAGE ?= hermes-web-retrieval-router:dev
MONOREPO_SOURCE ?= https://git.firewire.cc/michael/hermes-web-retrieval
MONOREPO_REVISION ?= $(shell git rev-parse HEAD)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
ROUTER_VERSION ?= dev
CRW_CANDIDATE_VERSION ?= 1.2.0-monorepo.$(shell git rev-parse --short=7 HEAD)
CRW_BUILD_IMAGE ?= hermes-web-retrieval-crw:$(CRW_CANDIDATE_VERSION)
# The combined monorepo gate favors bounded artifacts over debugger symbols.
# Component developers can still run the native crw/Makefile directly.
CRW_CARGO_ENV = CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=0 CARGO_PROFILE_TEST_DEBUG=0
HERMES_PYTHON ?= /usr/local/lib/hermes-agent/venv/bin/python
HERMES_REGRESSION_SEARCH_QUERY ?= Python programming language official documentation

help:
	@printf '%s\n' \
	  'Verification:' \
	  '  make check             Run all component and appliance checks' \
	  '  make test              Run all component and appliance tests' \
	  '  make check-router      Format, vet, test, and race-check the Go router' \
	  '  make check-crw         Run the CRW workspace checks' \
	  '  make test-appliance    Run Compose and backup/restore contracts' \
	  '  make check-updates     Compare reviewed refs with mutable upstreams' \
	  '  make test-hermes-regression  Run deterministic Hermes gate tests' \
	  '' \
	  'Build and operations:' \
	  '  make build-images      Build local router and CRW images' \
	  '  make up|down|ps        Manage the appliance in deploy/' \
	  '  make staging-up        Start an isolated candidate on port 33010' \
	  '  make staging-down      Remove isolated candidate containers' \
	  '  make smoke             Run the bounded production smoke test' \
	  '  make live-contract     Run search, scrape, cache, and concurrency gates'

check: check-router check-crw test-appliance test-hermes-regression compose-config check-stack-lock

check-router: router-fmt-check router-vet test-router router-race

check-crw:
	$(CRW_CARGO_ENV) $(MAKE) -C crw check

test: test-router test-crw test-appliance test-hermes-regression

test-router:
	$(GO_DOCKER) go test ./...

test-crw:
	$(CRW_CARGO_ENV) $(MAKE) -C crw test

test-appliance:
	python3 -m unittest discover -s tests/appliance -p '*_test.py' -v

test-hermes-regression:
	python3 -m unittest discover -s tests/live -p '*_test.py' -v

hermes-regression:
	@test -n "$(OUTPUT)" || { printf '%s\n' 'OUTPUT is required (for example: artifacts/hermes-regression-$$(date -u +%Y%m%dT%H%M%SZ).json)'; exit 2; }
	$(HERMES_PYTHON) scripts/hermes-regression-gate.py --hermes-python "$(HERMES_PYTHON)" --search-query "$(HERMES_REGRESSION_SEARCH_QUERY)" --output "$(OUTPUT)"

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
	docker build --pull \
		--build-arg VERSION=$(ROUTER_VERSION) \
		--build-arg REVISION=$(MONOREPO_REVISION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg SOURCE=$(MONOREPO_SOURCE) \
		-t $(ROUTER_IMAGE) router

build-crw-image:
	docker build --pull \
		--build-arg CRW_VERSION=$(CRW_CANDIDATE_VERSION) \
		--build-arg CRW_REVISION=$(MONOREPO_REVISION) \
		--build-arg CRW_BUILD_DATE=$(BUILD_DATE) \
		--build-arg CRW_SOURCE=$(MONOREPO_SOURCE) \
		-t $(CRW_BUILD_IMAGE) crw

build-images: build-router build-crw-image

up:
	ROUTER_REVISION=$(MONOREPO_REVISION) ROUTER_BUILD_DATE=$(BUILD_DATE) MONOREPO_SOURCE=$(MONOREPO_SOURCE) $(COMPOSE) up -d --build

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

staging-up:
	ROUTER_VERSION=$(STAGING_ROUTER_VERSION) ROUTER_REVISION=$(MONOREPO_REVISION) ROUTER_BUILD_DATE=$(BUILD_DATE) MONOREPO_SOURCE=$(MONOREPO_SOURCE) $(STAGING_COMPOSE) up -d --build

staging-down:
	$(STAGING_COMPOSE) down

staging-ps:
	$(STAGING_COMPOSE) ps

staging-smoke:
	./scripts/smoke-test.sh http://127.0.0.1:33010

staging-live-contract:
	ROUTER_URL=http://127.0.0.1:33010 ./scripts/live-contract-test.py

backup:
	./scripts/backup.sh "$(BACKUP)"

restore:
	./scripts/restore.sh --force "$(BACKUP)"

check-updates:
	./scripts/check-updates.sh check
