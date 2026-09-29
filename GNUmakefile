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

# Regenerate docs/ from the provider schema, templates/ and examples/.
# tfplugindocs is pinned as a Go tool in go.mod. It needs the terraform CLI:
# the one on PATH is used, otherwise the latest release is downloaded.
docs:
	terraform fmt -recursive examples/
	go tool tfplugindocs generate --provider-name sysutils

# Fail if docs/ is out of date with the schema, templates or examples, or if
# the generated docs or example formatting are invalid. Run by CI.
docs-check:
	terraform fmt -recursive -check -diff examples/
	go tool tfplugindocs generate --provider-name sysutils
	go tool tfplugindocs validate --provider-name sysutils
	@if [ -n "$$(git status --porcelain -- docs)" ]; then \
		echo 'docs/ is out of date. Run "make docs" and commit the result:'; \
		git status --porcelain -- docs; \
		git --no-pager diff -- docs; \
		exit 1; \
	fi

.PHONY: build install test testacc test-docker coverage lint docs docs-check
