# Package related
BINARY_NAME=ib-sriov
PACKAGE=ib-sriov-cni
BINDIR=$(CURDIR)/bin
BUILDDIR=$(CURDIR)/build
BASE=$(CURDIR)
GOFILES=$(shell find . -name *.go | grep -vE "(_test.go)")
PKGS=$(or $(PKG),$(shell $(GO) list ./...))
TESTPKGS = $(shell $(GO) list -f '{{ if or .TestGoFiles .XTestGoFiles }}{{ .ImportPath }}{{ end }}' $(PKGS))

# Version
VERSION?=master
DATE=`date -Iseconds`
COMMIT?=`git rev-parse --verify HEAD`
LDFLAGS="-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

# Docker
IMAGE_BUILDER?=docker
IMAGEDIR=$(BASE)/images
DOCKERFILE?=$(CURDIR)/Dockerfile
TAG?=k8snetworkplumbingwg/ib-sriov-cni
IMAGE_BUILD_OPTS?=
# Accept proxy settings for docker
# To pass proxy for Docker invoke it as 'make image HTTP_POXY=http://192.168.0.1:8080'
DOCKERARGS=
ifdef HTTP_PROXY
	DOCKERARGS += --build-arg http_proxy=$(HTTP_PROXY)
endif
ifdef HTTPS_PROXY
	DOCKERARGS += --build-arg https_proxy=$(HTTPS_PROXY)
endif
IMAGE_BUILD_OPTS += $(DOCKERARGS)

# Go tools
GO      = go
Q = $(if $(filter 1,$V),,@)
# Go settings
GO_BUILD_OPTS ?=CGO_ENABLED=0
GO_LDFLAGS ?=
GO_FLAGS ?=
GO_TAGS ?=-tags no_openssl
export GOPATH?=$(shell go env GOPATH)

# License targets
include make/license.mk

.PHONY: all
all: lint build test-coverage

$(BINDIR):
	@mkdir -p $@

$(BUILDDIR): ; $(info Creating build directory...)
	@mkdir -p $@

build: $(BUILDDIR)/$(BINARY_NAME) ; $(info Building $(BINARY_NAME)...) ## Build executable file
	$(info Done!)

$(BUILDDIR)/$(BINARY_NAME): $(GOFILES) | $(BUILDDIR)
	@cd $(BASE)/cmd/$(PACKAGE) && $(GO_BUILD_OPTS) $(GO) build -o $(BUILDDIR)/$(BINARY_NAME) $(GO_TAGS) -ldflags $(LDFLAGS) -v

# Tools

GOLANGCI_LINT = $(BINDIR)/golangci-lint
# golangci-lint version should be updated periodically
# we keep it fixed to avoid it from unexpectedly failing on the project
# in case of a version bump
GOLANGCI_LINT_VER = v1.64.8
TIMEOUT = 15
export GOLANGCI_LINT_CACHE = $(BUILDDIR)/.cache

$(GOLANGCI_LINT): | $(BINDIR) ; $(info  installing golangci-lint...)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VER))

GOVERALLS = $(BINDIR)/goveralls
$(GOVERALLS): | $(BINDIR) ; $(info  installing goveralls...)
	$(call go-install-tool,$(GOVERALLS),github.com/mattn/goveralls@latest)

HADOLINT_TOOL = $(BINDIR)/hadolint
$(HADOLINT_TOOL): | $(BINDIR) ; $(info  installing hadolint...)
	$(call wget-install-tool,$(HADOLINT_TOOL),"https://github.com/hadolint/hadolint/releases/download/v2.12.1-beta/hadolint-Linux-x86_64")

SHELLCHECK_TOOL = $(BINDIR)/shellcheck
$(SHELLCHECK_TOOL): | $(BASE) ; $(info  installing shellcheck...)
	$(call install-shellcheck,$(BINDIR),"https://github.com/koalaman/shellcheck/releases/download/v0.9.0/shellcheck-v0.9.0.linux.x86_64.tar.xz")

# Tests

.PHONY: lint
lint: | $(GOLANGCI_LINT) ; $(info  running golangci-lint...) ## Run golangci-lint
	$Q $(GOLANGCI_LINT) run --timeout=5m

TEST_TARGETS := test-default test-bench test-short test-verbose test-race
.PHONY: $(TEST_TARGETS) test tests
test-bench:   ARGS=-run=__absolutelynothing__ -bench=. ## Run benchmarks
test-short:   ARGS=-short        ## Run only short tests
test-verbose: ARGS=-v            ## Run tests in verbose mode with coverage reporting
test-race:    ARGS=-race         ## Run tests with race detector
$(TEST_TARGETS): NAME=$(MAKECMDGOALS:test-%=%)
$(TEST_TARGETS): test
test: ; $(info  running $(NAME:%=% )tests...) @ ## Run tests
	$Q $(GO) test -timeout $(TIMEOUT)s $(ARGS) $(TESTPKGS)

COVERAGE_MODE = count
COVER_PROFILE = ib-sriov-cni.cover
test-coverage: | $(GOVERALLS) ; $(info  running coverage tests...) ## Run coverage tests
	$Q $(GO) test -covermode=$(COVERAGE_MODE) -coverprofile=$(COVER_PROFILE) ./...

.PHONY: upload-coverage
upload-coverage: | $(GOVERALLS) ; $(info  uploading coverage results...) ## Upload coverage report
	$(GOVERALLS) -coverprofile=$(COVER_PROFILE) -service=github

.PHONY: hadolint
hadolint: $(HADOLINT_TOOL); $(info  running hadolint...) ## Run hadolint
	$Q $(HADOLINT_TOOL) Dockerfile

.PHONY: shellcheck
shellcheck: $(SHELLCHECK_TOOL); $(info  running shellcheck...) ## Run shellcheck
	$Q $(SHELLCHECK_TOOL) images/entrypoint.sh

# Container image
.PHONY: image
image: ; $(info Building Docker image...)  ## Build conatiner image
	@$(IMAGE_BUILDER) build -t $(TAG) -f $(DOCKERFILE)  $(CURDIR) $(IMAGE_BUILD_OPTS)

# Dependency management
.PHONY: deps-update
deps-update: ; $(info  updating dependencies...) ## update dependencies by running go mod tidy
	go mod tidy

.PHONY: test-image
test-image: image ## Test image
	$Q $(BASE)/images/image_test.sh $(IMAGE_BUILDER) $(TAG)

tests: lint hadolint shellcheck test test-image ## Run lint, hadolint, shellcheck, unit test and image test

# Misc

.PHONY: clean
clean: ; $(info  Cleaning...) ## Cleanup everything
	@$(GO) clean -modcache
	@rm -rf $(BINDIR)
	@rm -rf $(BUILDDIR)
	@rm -rf  test

.PHONY: help
help: ## Show this message
	@grep -E '^[ a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

define wget-install-tool
@[ -f $(1) ] || { \
echo "Downloading $(2)" ;\
mkdir -p $(BINDIR);\
wget -O $(1) $(2);\
chmod +x $(1) ;\
}
endef

define install-shellcheck
@[ -f $(1) ] || { \
echo "Downloading $(2)" ;\
mkdir -p $(1);\
wget -O $(1)/shellcheck.tar.xz $(2);\
tar xf $(1)/shellcheck.tar.xz -C $(1);\
mv $(1)/shellcheck*/shellcheck $(1)/shellcheck;\
chmod +x $(1)/shellcheck;\
rm -r $(1)/shellcheck*/;\
rm $(1)/shellcheck.tar.xz;\
}
endef

# go-install-tool will 'go install' any package $2 and install it to $1.
define go-install-tool
@[ -f $(1) ] || { \
set -e ;\
echo "Downloading $(2)" ;\
GOBIN=$(BINDIR) go install -mod=mod $(2) ;\
}
endef
