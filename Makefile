.PHONY: lint test fmt clean

export GO111MODULE=on

default: fmt lint test

fmt:
	go fmt ./...

lint:
	golangci-lint run

test:
	go test -race -cover ./...

clean:
	rm -f coverage.out
