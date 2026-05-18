GOBIN  ?= $(CURDIR)/bin
GOCMD   = go
LDFLAGS = -ldflags="-s -w"

.PHONY: build test clean

build:
	@mkdir -p $(GOBIN)
	$(GOCMD) build $(LDFLAGS) -o $(GOBIN)/magento-compatibility-builder ./cmd/builder
	$(GOCMD) build $(LDFLAGS) -o $(GOBIN)/magento-compatibility ./cmd/cli

test:
	$(GOCMD) test ./...

clean:
	rm -rf $(GOBIN)
