# Export SPIRE_DEV_HOST to docker to build SPIRE using a docker container
ifeq ($(SPIRE_DEV_HOST), docker)
	docker = docker run -v $(docker_volume_spire) -v $(docker_volume_gopath) -it $(docker_image)
	container = container
else
	docker =
	container =
endif

# Enable go modules
export GO111MODULE=on

# Makefile variables
binary_dirs := $(shell find cmd/* functional/tools/* -maxdepth 0 -type d)
docker_volume_gopath := $(shell echo $${GOPATH}/pkg/mod):/root/go/pkg/mod
docker_volume_spire := $(shell echo $${PWD}):/root/spire
docker_image = spire-dev:latest
gopath := $(shell go env GOPATH)
gittag := $(shell git tag --points-at HEAD)
gitdirty := $(shell git status -s)
# don't provide the git tag if the git status is dirty.
ifneq ($(gitdirty),)
	gittag :=
endif
ldflags := '-X github.com/spiffe/spire/pkg/common/version.gittag=$(gittag)'

utils = github.com/golang/protobuf/protoc-gen-go \
		github.com/grpc-ecosystem/grpc-gateway \
		github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway \
		github.com/grpc-ecosystem/grpc-gateway/protoc-gen-swagger \
		github.com/jteeuwen/go-bindata/go-bindata

# Help message settings
cyan := $(shell tput setaf 6)
reset := $(shell tput sgr0)
bold  := $(shell tput bold)
target_max_char=25

# Makefile options
.PHONY: all utils container-push cmd build test race-test clean functional vendor help


# Make targets
##@ Building
build: $(binary_dirs) ## Build SPIRE binaries

$(binary_dirs): noop
	$(docker) /bin/sh -c "cd $@; go build -ldflags $(ldflags)"

all: $(container) build test ## Build and run tests


##@ Testing
test: ## Run tests
	$(docker) go test -race -timeout 8m github.com/spiffe/spire/...

race-test: ## Run race tests
	$(docker) go test -race github.com/spiffe/spire/...

integration: ## Run integration tests
	$(docker) script/e2e_test.sh

functional: ## Run functional tests
	$(MAKE) -C functional/ all


##@ Cleaning
clean: ## Go-clean object files
	$(docker) go clean github.com/spiffe/spire/...

distclean: clean ## Remove object files, vendor and .cache folders
	rm -rf .cache
	rm -rf vendor


##@ Container
container: Dockerfile ## Build Docker container for compilation
	docker build -t $(docker_image) --no-cache .

container-push: ## Push docker container image
	docker tag $(docker_image) spiffe/$(docker_image)
	docker push spiffe/$(docker_image)

cmd: ## Opens a shell in docker container
	$(docker) /bin/bash


##@ Others
utils: $(utils) ## Go-get SPIRE utils

$(utils): noop
	# some sources do not contain buildable go, hence || true
	# Since 'go get' behaves differently in 'modules-mode', it is disabled to install tools globally.
	$(docker) /bin/sh -c "GO111MODULE=off go get $@ || true"

# Vendor is not needed for building. It is just kept for compatibility with IDEs that does not support modules yet.
vendor: ## Make vendored copy of dependencies.
	$(docker) go mod vendor

artifact: ## Build SPIRE artifacts
	$(docker) ./build.sh artifact

noop:

help: ## Show this help message.
	@awk 'BEGIN {FS = ":.*##"; printf "\n$(bold)Usage:$(reset) make $(cyan)<target>$(reset)\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  $(cyan)%-$(target_max_char)s$(reset) %s\n", $$1, $$2 } /^##@/ { printf "\n $(bold)%s$(reset) \n", substr($$0, 5) } ' $(MAKEFILE_LIST)