VERSION := $(shell grep -o 'version = "[^"]*"' main.go | cut -d'"' -f2)
LDFLAGS := -s -w
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build build-all run vet

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o gafyi .

build-all:
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; [ "$$os" = windows ] && ext=".exe"; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags="$(LDFLAGS)" -o dist/gafyi-$(VERSION)-$$os-$$arch$$ext . ; \
	done

run: build
	./gafyi -config config.json

vet:
	gofmt -l . && go vet ./...
