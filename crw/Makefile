.PHONY: hooks fmt clippy test build check live-compat sync-docs-changelog

# Install git pre-commit hook
hooks:
	@cp scripts/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Pre-commit hook installed."

# Individual CI-equivalent targets
fmt:
	cargo fmt --all

fmt-check:
	cargo fmt --all -- --check

clippy:
	cargo clippy --workspace --all-targets -- -D warnings

test:
	cargo test --workspace

build:
	cargo build --workspace --all-targets

# Run all checks (same as CI)
check: fmt-check clippy test

live-compat:
	python3 scripts/live_compatibility.py --phase all --output live-compatibility.json

sync-docs-changelog:
	python3 scripts/sync-docs-changelog.py
