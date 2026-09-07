# The shipped artefact is one file. Everything here builds that file.
BIN := bin/governor

.PHONY: build test vet fmt check clean cross

build:
	go build -o $(BIN) ./cmd/governor

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

check: vet test
	@unformatted=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi

# What CI builds, minus the runner split: cgo is off here because a local cross-compile
# cannot link another platform's C toolchain. CI builds darwin natively with cgo on.
cross:
	@for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -o bin/governor-$$os-$$arch ./cmd/governor || exit 1; \
	done

clean:
	go clean
	rm -rf bin
