NAME := quadstack
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
RELEASE_TARGETS := linux/amd64 linux/arm64

.PHONY: build web linux-amd64 release test clean

web:
	cd web && npm ci && npm run build

build: web
	go build -ldflags="-X main.version=$(VERSION)" -o $(NAME) .

linux-amd64: web
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$(NAME)_linux_amd64 .

release: web
	rm -rf dist && mkdir -p dist
	@for t in $(RELEASE_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "building $${os}_$${arch}"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$(NAME)_$${os}_$${arch} . || exit 1; \
	done
	cd dist && shasum -a 256 $(NAME)_* > SHA256SUMS

test:
	go vet ./...
	go test ./...
	cd web && npx svelte-check --tsconfig ./tsconfig.json

clean:
	rm -rf dist $(NAME) web/dist
