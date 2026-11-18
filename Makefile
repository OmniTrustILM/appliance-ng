.PHONY: addons images test lint boot-test generate manifests api-gen web

ENVTEST_K8S_VERSION ?= 1.37.x
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0
OAPI_CODEGEN ?= go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.2

addons:
	./build/addons.sh

# make images ARCH=amd64|arm64 REGISTRY=<registry>; defaults in build/lib.sh.
images:
	./build/extension.sh
	./build/manager.sh
	./build/installer.sh
	./build/cache.sh
	./build/media.sh

# make boot-test MEDIA=iso|qcow2 ARCH=arm64|amd64 boots the image built by make images.
MEDIA ?= iso
ARCH ?= arm64
boot-test:
	./test/boot/run.sh $(MEDIA) $(ARCH)

# Regenerate the deep-copy code and the CRD of the Appliance API.
generate:
	$(CONTROLLER_GEN) object paths=./api/...

# Regenerate the CRD and addons/manager.yaml, which embeds it, and pin its sha256 in release.yaml.
manifests:
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd
	./build/manifests.sh

# Regenerate the Go servers and the TypeScript types from the API contract; web needs npm ci first.
api-gen:
	$(OAPI_CODEGEN) -config internal/api/oapi-codegen.yaml -o internal/api/gen.go api/openapi.yaml
	$(OAPI_CODEGEN) -config internal/api/setupapi/oapi-codegen.yaml -o internal/api/setupapi/gen.go api/openapi.yaml
	cd web && npm run api-gen

# Build the console into web/dist/app, which the binaries embed.
web:
	cd web && npm ci && npm run build

test:
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.0.0-20260125163108-a19ec76a3c5d use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./... -coverprofile=coverage.out
	cd web && npm test -- --coverage

lint:
	golangci-lint run
