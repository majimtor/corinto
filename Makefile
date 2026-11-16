# The single entry point of the repository (ADR 0015). CI runs these same
# targets, so what passes here passes there.

# Command-line tools that CI and local runs share, pinned in this one place
# (docs/engineering-rules.md, "Environment and lockfiles"). Container images
# carry their digest; Go tools are built by the toolchain of go.mod at the
# version named here. Update one tool at a time.
GITLEAKS_IMAGE := ghcr.io/gitleaks/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f
MARKDOWNLINT_IMAGE := davidanson/markdownlint-cli2:v0.24.0@sha256:70ead7c0352d22d04bbe612cd96b54ec591538d8f4626cafe391bb2ecf3639ca
STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0

.PHONY: test verify lint secrets docs

# Runs the unit tests with the race detector, in random order and without the
# test cache, so that a pass means the tests ran.
test:
	go test -race -shuffle=on -count=1 ./...

# Runs every check CI runs. A clean clone must pass it.
verify: lint test secrets docs

# Compiles everything and runs the static analysers.
lint:
	go build ./...
	go vet ./...
	$(STATICCHECK) ./...
	$(GOLANGCI_LINT) run

# Scans the whole history for secrets. The pre-commit hook looks at what is
# staged; this finds what got past it. The repository is mounted read-only
# and marked safe, because the container runs as another user.
secrets:
	docker run --rm --volume "$(CURDIR):/repo:ro" \
		--env GIT_CONFIG_COUNT=1 \
		--env GIT_CONFIG_KEY_0=safe.directory --env GIT_CONFIG_VALUE_0=/repo \
		$(GITLEAKS_IMAGE) git --redact --no-banner --exit-code 1 /repo

# Lints every Markdown file with the configuration in .markdownlint-cli2.yaml.
docs:
	docker run --rm --volume "$(CURDIR):/workdir:ro" $(MARKDOWNLINT_IMAGE) "**/*.md"
