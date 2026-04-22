default: build

build:
	go build -o terraform-provider-sysutils

install: build
	mkdir -p ~/.terraform.d/plugins/registry.terraform.io/blechschmidt/sysutils/0.1.0/linux_amd64
	cp terraform-provider-sysutils ~/.terraform.d/plugins/registry.terraform.io/blechschmidt/sysutils/0.1.0/linux_amd64/

test:
	go test ./... -timeout 30m

testacc:
	TF_ACC=1 go test ./... -v -timeout 30m

# Run integration tests inside a throwaway Docker container so that useradd,
# file writes, and command execution cannot affect the host system.
test-docker:
	docker build -f Dockerfile.test -t terraform-provider-sysutils-tests .
	docker run --rm terraform-provider-sysutils-tests

coverage:
	go test ./... -coverprofile=coverage.out -timeout 30m
	go tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

.PHONY: build install test testacc test-docker coverage lint
