# The shipped artefact is one file. Everything here builds that file.
BIN := bin/governor

.PHONY: build test vet fmt check clean cross leakcheck

build:
	go build -o $(BIN) ./cmd/governor

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

# A test that leaves a daemon running is a test that degrades the machine it measures, and
# the residue is invisible: a governor on a scratch socket collides with nothing and is not a
# descendant of any `claude`, so neither `doctor` nor the Phase 0 sampler can see it (GOV-B11).
leakcheck:
	./tools/leakcheck.sh

check: vet test
	@unformatted=$$(gofmt -l ./cmd ./internal); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi

# What CI builds, minus the runner split: cgo is off here because a local cross-compile
# cannot link another platform's C toolchain. CI builds darwin natively with cgo on.
#
# The darwin binaries this produces are thermally blind — no cgo means no
# ProcessInfo.thermalState shim, so they report "unknown" on a Mac and look exactly like a
# cool machine (design.md section 5.3). This target is a compile check for all four targets;
# do NOT ship its darwin output. Release artefacts come from the macOS CI runner.
cross:
	@for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -o bin/governor-$$os-$$arch ./cmd/governor || exit 1; \
	done

clean:
	go clean
	rm -rf bin
