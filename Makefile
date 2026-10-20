.PHONY: build build-cli run clean test test-coverage lint docs docs-dev sandbox sandbox-down sandbox-status test-scripts

build:
	CGO_ENABLED=1 go build -o bin/ah ./cmd/ah

build-cli:
	go build -o bin/ahc ./cmd/ahc

run: build
	./bin/ah --port 8080 --db-path /var/lib/ah/ah.db --master-key-path /var/lib/ah/master.key

test:
	CGO_ENABLED=1 go test ./... -timeout 60s

test-coverage:
	CGO_ENABLED=1 go test ./... -coverprofile=coverage.out -timeout 60s
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Throwaway local instance (docs/runbooks/local-sandbox.md). Pass flags with
# SANDBOX_FLAGS, e.g. make sandbox SANDBOX_FLAGS=--yes
sandbox:
	scripts/sandbox.sh up $(SANDBOX_FLAGS)

sandbox-down:
	scripts/sandbox.sh down $(SANDBOX_FLAGS)

sandbox-status:
	scripts/sandbox.sh status

# Dry-run/help checks for the setup and sandbox scripts (no sudo, no installs).
test-scripts:
	scripts/test-setup-scripts.sh
	scripts/test-bootstrap-macmini.sh
	scripts/bootstrap-stack.sh --self-test

docs:
	cd docs-site && npm ci && npm run llms && npm run build

docs-dev:
	cd docs-site && npm install && npm run dev

clean:
	rm -rf bin/ coverage.out coverage.html docs-site/dist docs-site/.astro
