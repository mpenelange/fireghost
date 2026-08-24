.PHONY: test test-python fmt vet race compose-config build smoke live-contract backup check-updates

GO_DOCKER = docker run --rm -v "$(CURDIR):/src" -w /src golang:1.24.6-bookworm@sha256:ab1d1823abb55a9504d2e3e003b75b36dbeb1cbcc4c92593d85a84ee46becc6c

test: test-python
	$(GO_DOCKER) go test ./...

test-python:
	python3 -m unittest discover -s tests -p '*_test.py' -v

fmt:
	$(GO_DOCKER) sh -c 'test -z "$$(gofmt -l .)"'

vet:
	$(GO_DOCKER) go vet ./...

race:
	$(GO_DOCKER) go test -race ./...

compose-config:
	docker compose config --quiet

build:
	docker build --pull -t web-retrieval-router:dev .

smoke:
	./scripts/smoke-test.sh

live-contract:
	./scripts/live-contract-test.py

backup:
	./scripts/backup.sh "$(BACKUP)"

check-updates:
	./scripts/check-updates.sh

