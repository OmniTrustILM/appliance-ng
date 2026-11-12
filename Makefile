.PHONY: addons images test lint boot-test

ENVTEST_K8S_VERSION ?= 1.37.x

addons:
	./build/addons.sh

# make images ARCH=amd64|arm64 REGISTRY=<registry>; defaults in build/lib.sh.
images:
	./build/extension.sh
	./build/installer.sh
	./build/cache.sh
	./build/media.sh

# make boot-test MEDIA=iso|qcow2 ARCH=arm64|amd64 boots the image built by make images.
MEDIA ?= iso
ARCH ?= arm64
boot-test:
	./test/boot/run.sh $(MEDIA) $(ARCH)

test:
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.0.0-20260125163108-a19ec76a3c5d use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./... -coverprofile=coverage.out

lint:
	golangci-lint run
