# blind-llm-eyes Makefile.
# Dev convenience targets. The release artifact is produced by goreleaser
# (see .goreleaser.yaml), invoked either locally via `make release` or by
# the .github/workflows/release.yml workflow on tag push.

BINARY := blind-llm-eyes
VERSION ?= dev
LDFLAGS := -s -w -X github.com/ROM4n2/blind-llm-eyes/buildinfo.Version=$(VERSION)

.PHONY: test vet build run snapshot release clean

# Run the full test suite with the race detector (the project's CI gate).
test:
	go test -race -count=1 ./...

# Static checks.
vet:
	go vet ./...

# Build a local binary with the given VERSION (default "dev").
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

# Build and run the proxy in the foreground (server path).
run: build
	./$(BINARY)

# goreleaser dry-run: compile all platform targets without publishing.
snapshot:
	goreleaser build --snapshot --clean

# Validate the goreleaser config syntax.
goreleaser-check:
	goreleaser check

# Publish a real release. Requires a git tag and GITHUB_TOKEN; normally
# triggered by CI on tag push, but available locally for maintainers.
release: clean
	goreleaser release --clean

# Remove build artifacts.
clean:
	rm -rf dist/ $(BINARY) $(BINARY).exe
