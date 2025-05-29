##### Global variables #####
include version.mk Makefile.defs

all: build

docker:
	docker build \
	--build-arg GOLANG_IMAGE=${GOLANG_IMAGE} \
	--build-arg TARGET_ARCH=${TARGET_ARCH} \
	--build-arg NVIDIA_IMAGE=${NVIDIA_IMAGE} \
	--build-arg DEST_DIR=${DEST_DIR} \
	--build-arg VERSION=${VERSION} \
	--build-arg GOPROXY=https://goproxy.cn,direct \
	. -f=docker/Dockerfile -t ${IMG_TAG}

dockerwithlib:
	docker build \
	--no-cache \
	--build-arg GOLANG_IMAGE=${GOLANG_IMAGE} \
	--build-arg TARGET_ARCH=${TARGET_ARCH} \
	--build-arg NVIDIA_IMAGE=${NVIDIA_IMAGE} \
	--build-arg DEST_DIR=${DEST_DIR} \
	--build-arg VERSION=${VERSION} \
	--build-arg GOPROXY=https://goproxy.cn,direct \
	. -f=docker/Dockerfile.withlib -t ${IMG_TAG}

tidy:
	$(GO) mod tidy

proto:
	$(GO) get github.com/gogo/protobuf/protoc-gen-gofast@v1.3.2
	protoc --gofast_out=plugins=grpc:. ./pkg/api/*.proto

build: $(CMDS) $(DEVICES)

$(CMDS):
	$(GO) build -ldflags '-s -w -X github.com/Project-HAMi/HAMi/pkg/version.version=$(VERSION)' -o ${OUTPUT_DIR}/$@ ./cmd/$@

$(DEVICES):
	$(GO) build -ldflags '-s -w -X github.com/Project-HAMi/HAMi/pkg/device-plugin/nvidiadevice/nvinternal/info.version=$(VERSION)' -o ${OUTPUT_DIR}/$@-device-plugin ./cmd/device-plugin/$@

clean:
	$(GO) clean -r -x ./cmd/...
	-rm -rf $(OUTPUT_DIR)

.PHONY: all build docker clean test $(CMDS)

test:
	mkdir -p ./_output/coverage/
	bash hack/unit-test.sh

lint:
	bash hack/verify-staticcheck.sh

.PHONY: verify
verify:
	hack/verify-all.sh

.PHONY: lint_dockerfile
lint_dockerfile:
	@ docker run --rm \
          -v $(ROOT_DIR)/.trivyignore:/.trivyignore \
          -v /tmp/trivy:/root/trivy.cache/  \
          -v $(ROOT_DIR):/tmp/src  \
          aquasec/trivy:$(TRIVY_VERSION) config --exit-code 1  --severity $(LINT_TRIVY_SEVERITY_LEVEL) /tmp/src/docker  ; \
      (($$?==0)) || { echo "error, failed to check dockerfile trivy" && exit 1 ; } ; \
      echo "dockerfile trivy check: pass"

.PHONY: lint_chart
lint_chart:
	@ docker run --rm \
          -v $(ROOT_DIR)/.trivyignore:/.trivyignore \
          -v /tmp/trivy:/root/trivy.cache/  \
          -v $(ROOT_DIR):/tmp/src  \
          aquasec/trivy:$(TRIVY_VERSION) config --exit-code 1  --severity $(LINT_TRIVY_SEVERITY_LEVEL) /tmp/src/charts  ; \
      (($$?==0)) || { echo "error, failed to check chart trivy" && exit 1 ; } ; \
      echo "chart trivy check: pass"

.PHONY: e2e-env-setup
e2e-env-setup:
	./hack/e2e-test-setup.sh

.PHONY: helm-deploy
helm-deploy:
	./hack/deploy-helm.sh "${E2E_TYPE}" "${KUBE_CONF}" "${HAMI_VERSION}"

.PHONY: e2e-test
e2e-test:
	./hack/e2e-test.sh "${E2E_TYPE}" "${KUBE_CONF}"

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.17.2

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) crd paths="./pkg/api/..." output:crd:artifacts:config=charts/hami/crds

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object paths="./..."

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef