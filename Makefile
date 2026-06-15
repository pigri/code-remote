# code-remote — build the binaries and install the crctl CLI.
#
#   make            # build all binaries into ./bin
#   sudo make install   # install crctl -> /usr/local/bin
#   make deploy     # install server + ngrok-forward into the systemd runtime dir

GO       ?= go
BINDIR   ?= bin
PREFIX   ?= /usr/local
RUNDIR   ?= $(HOME)/.local/share/code-remote
INSTALL  ?= install
VERSION  ?= $(shell git describe --tags --always 2>/dev/null | sed 's/^v//' || echo 0.0.0-dev)
DEB_ARCH ?= $(shell dpkg --print-architecture 2>/dev/null || echo amd64)

# All non-test Go sources (incl. internal/) so a change anywhere triggers a
# rebuild. The targets are also .PHONY: go's build cache keeps this cheap, and
# it avoids stale binaries when only internal packages change.
GO_SRC := $(shell find . -name '*.go' -not -name '*_test.go') go.mod go.sum

.PHONY: all build install uninstall deploy deb test vet fmt tidy clean \
        $(BINDIR)/claude-remote-api $(BINDIR)/crctl $(BINDIR)/ngrok-forward

all: build

build: $(BINDIR)/claude-remote-api $(BINDIR)/crctl $(BINDIR)/ngrok-forward

$(BINDIR)/claude-remote-api: $(GO_SRC)
	@mkdir -p $(BINDIR)
	$(GO) build -o $@ .

$(BINDIR)/crctl: $(GO_SRC)
	@mkdir -p $(BINDIR)
	$(GO) build -o $@ ./cmd/crctl

$(BINDIR)/ngrok-forward: $(GO_SRC)
	@mkdir -p $(BINDIR)
	$(GO) build -o $@ ./cmd/ngrok-forward

## install: install the crctl CLI to $(PREFIX)/bin (run with sudo)
install: $(BINDIR)/crctl
	$(INSTALL) -d $(DESTDIR)$(PREFIX)/bin
	$(INSTALL) -m 0755 $(BINDIR)/crctl $(DESTDIR)$(PREFIX)/bin/crctl
	@echo "installed crctl -> $(DESTDIR)$(PREFIX)/bin/crctl"

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/crctl

## deploy: install the server + ngrok-forward into the systemd runtime dir
deploy: $(BINDIR)/claude-remote-api $(BINDIR)/ngrok-forward
	$(INSTALL) -d $(RUNDIR)
	$(INSTALL) -m 0755 $(BINDIR)/claude-remote-api $(RUNDIR)/
	$(INSTALL) -m 0755 $(BINDIR)/ngrok-forward $(RUNDIR)/

## deb: build a .deb (code-remote_$(VERSION)_$(DEB_ARCH).deb) into ./dist
deb: build
	VERSION=$(VERSION) ARCH=$(DEB_ARCH) BIN_DIR=$(BINDIR) sh packaging/deb/build.sh dist

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BINDIR) dist
