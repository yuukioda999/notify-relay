.PHONY: run test lint vuln build

run:
	go run ./cmd/notify-relay

test:
	go test -race -cover ./...

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

build:
	docker build -t notify-relay:local .
