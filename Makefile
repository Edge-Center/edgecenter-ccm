VERSION=v2.0.3-beta
ARCH=amd64
GOOS=linux
PLATFORM=linux/amd64
REGISTRY=harbor.mkaas.edgecenter.online/docker/ec-ccm
BINARY=bin/ec-ccm

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(ARCH) go build -o $(BINARY) cmd/main.go

image-build: build
	docker build --platform linux/amd64 -t $(REGISTRY):$(VERSION) .

image-push: image-build
	docker push $(REGISTRY):$(VERSION)