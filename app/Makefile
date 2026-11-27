GOBIN  ?= $(CURDIR)/bin
GOCMD   = go
LDFLAGS = -ldflags="-s -w"

.PHONY: build test clean

build:
	@mkdir -p $(GOBIN)
	$(GOCMD) build $(LDFLAGS) -o $(GOBIN)/magento-compatibility-builder ./cmd/builder
	$(GOCMD) build $(LDFLAGS) -o $(GOBIN)/magento-compatibility ./cmd/cli
	$(GOCMD) build $(LDFLAGS) -o $(GOBIN)/m2test-worker ./cmd/worker

test:
	$(GOCMD) test ./...

clean:
	rm -rf $(GOBIN)
