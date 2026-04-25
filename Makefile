VERSION ?= 1.2.2
BINDIR = bin
LDFLAGS = -s -w -X main.version=$(VERSION)
GOFLAGS = -trimpath
export CGO_ENABLED = 0

.PHONY: all build test clean dist linux-amd64 linux-arm64 macos-amd64 macos-arm64 windows-amd64

all: build

build:
	go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BINDIR)/xfer ./cmd/xfer

test:
	go test ./...

clean:
	rm -rf $(BINDIR)

dist: clean linux-amd64 linux-arm64 macos-amd64 macos-arm64 windows-amd64

linux-amd64:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BINDIR)/xfer-linux-amd64 ./cmd/xfer

linux-arm64:
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BINDIR)/xfer-linux-arm64 ./cmd/xfer

macos-amd64:
	GOOS=darwin GOARCH=amd64 go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BINDIR)/xfer-macos-amd64 ./cmd/xfer

macos-arm64:
	GOOS=darwin GOARCH=arm64 go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BINDIR)/xfer-macos-arm64 ./cmd/xfer

windows-amd64:
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BINDIR)/xfer-windows-amd64.exe ./cmd/xfer
