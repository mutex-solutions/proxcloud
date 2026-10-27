REGISTRY ?= ghcr.io/mutex-solutions
TAG      ?= go-m1-$(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
CHART    ?= deploy/charts/proxcloud
NS       ?= proxcloud
# kube context for `make deploy`; empty = kubectl's current context.
KUBECTX  ?=
# Optional env-specific values overlay (gitignored, bring your own): make deploy VALUES=deploy/my-values.yaml
VALUES   ?=
IMAGE    := $(REGISTRY)/proxcloud-api:$(TAG)

# One-line recipes (`target: ; cmd`) avoid Makefile tab pitfalls.
.PHONY: build vet test test-integration binary image push deploy print-image

build:   ; go build ./...
vet:     ; go vet ./...
test:    ; go test ./...
# Hermetic integration tests against a throwaway PostgreSQL (testcontainers; needs Docker).
test-integration: ; go test -tags=integration ./test/integration/...
binary:  ; CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/proxcloud-api ./cmd/api
image:   ; docker build -f deploy/Dockerfile -t $(IMAGE) .
push:    ; docker push $(IMAGE)
deploy:  ; helm upgrade --install proxcloud $(CHART) -n $(NS) --create-namespace $(if $(KUBECTX),--kube-context $(KUBECTX)) $(if $(VALUES),-f $(VALUES)) --set api.image.tag=$(TAG)
print-image: ; @echo $(IMAGE)
