.PHONY: build run clean test test-coverage lint

build:
	CGO_ENABLED=1 go build -o bin/ah ./cmd/ah

run: build
	./bin/ah --port 8080 --db-path /var/lib/ah/ah.db --master-key-path /var/lib/ah/master.key

test:
	CGO_ENABLED=1 go test ./... -timeout 60s

test-coverage:
	CGO_ENABLED=1 go test ./... -coverprofile=coverage.out -timeout 60s
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

clean:
	rm -rf bin/ coverage.out coverage.html
