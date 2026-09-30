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

# Run the full suite, including acceptance tests, as root inside a throwaway
# Docker container so that useradd, chown, file writes and command execution
# cannot affect the host. TF_CLI is terraform or tofu; TF_CLI_VERSION is an
# exact release, a prefix such as 1.5 (newest 1.5.x) or latest. CI runs this
# for every entry of its CLI matrix. SYS_ADMIN and an unconfined AppArmor
# profile let the tests mount a tmpfs inside the container's own mount
# namespace; the host's mounts are not affected. NET_ADMIN lets the firewall
# rule tests change the firewall of network namespaces they create inside
# the container; the host's firewall is not affected either.
#
# SYSUTILS_UPGRADE_FROM_REF=auto (or a git ref) also runs the upgrade tests
# from a local baseline build (internal/provider/upgrade_local_acc_test.go).
# They build the provider at an earlier commit, so the repository's .git
# directory is mounted read-only into the container; it needs the full
# history.
TF_CLI ?= terraform
TF_CLI_VERSION ?= latest
TESTACC_IMAGE = terraform-provider-sysutils-testacc:$(TF_CLI)-$(TF_CLI_VERSION)
SYSUTILS_UPGRADE_FROM_REF ?=
TESTACC_UPGRADE_ARGS = $(if $(SYSUTILS_UPGRADE_FROM_REF),-e SYSUTILS_UPGRADE_FROM_REF=$(SYSUTILS_UPGRADE_FROM_REF) -v $(CURDIR)/.git:/workspace/.git:ro)

testacc-docker:
	docker build -f Dockerfile.test \
		--build-arg TF_CLI=$(TF_CLI) \
		--build-arg TF_CLI_VERSION=$(TF_CLI_VERSION) \
		-t $(TESTACC_IMAGE) .
	docker run --rm --cap-add SYS_ADMIN --cap-add NET_ADMIN --security-opt apparmor=unconfined \
		$(TESTACC_UPGRADE_ARGS) $(TESTACC_IMAGE)

# Run testacc-docker for every CLI in the CI matrix.
testacc-docker-matrix:
	$(MAKE) testacc-docker TF_CLI=terraform TF_CLI_VERSION=1.5
	$(MAKE) testacc-docker TF_CLI=terraform TF_CLI_VERSION=latest
	$(MAKE) testacc-docker TF_CLI=tofu TF_CLI_VERSION=latest

# Kept for existing scripts and docs.
test-docker: testacc-docker

# Apply examples/complete with the locally built provider (dev_overrides),
# check that a second plan is empty, then destroy it. Creates a user, a group,
# a systemd unit and a sysctl.d file on the host: requires root and is meant
# for CI runners and throwaway VMs. TF_CLI selects terraform or tofu.
e2e:
	TF_CLI=$(TF_CLI) scripts/e2e-complete.sh

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

.PHONY: build install test testacc testacc-docker testacc-docker-matrix test-docker e2e coverage lint docs docs-check
