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

# The compose project is named in deploy/compose.yaml. `down` goes by that
# name, so that it works even when deploy/.env is gone.
COMPOSE := docker compose --file deploy/compose.yaml
COMPOSE_PROJECT := corinto
IMAGE := ghcr.io/majimtor/corinto
API_URL := http://127.0.0.1:8080
HEALTH_URL := http://127.0.0.1:8081
REDIRECT_URL := http://127.0.0.1:8082
REDIRECT_HEALTH_URL := http://127.0.0.1:8083
WORKER_HEALTH_URL := http://127.0.0.1:8084

.PHONY: up down seed test integration verify lint secrets docs image

# Starts the whole system on this machine and waits until it reports ready.
# The first start seeds two demo workspaces and prints a key for each, this
# once: only a hash of a key is stored. A later start leaves them as they
# are and prints no key.
up: deploy/.env
	$(COMPOSE) up --build --detach --wait
	@curl --silent --show-error --fail --output /dev/null \
		--retry 30 --retry-delay 1 --retry-all-errors $(HEALTH_URL)/readyz
	@curl --silent --show-error --fail --output /dev/null \
		--retry 30 --retry-delay 1 --retry-all-errors $(REDIRECT_HEALTH_URL)/readyz
	@curl --silent --show-error --fail --output /dev/null \
		--retry 30 --retry-delay 1 --retry-all-errors $(WORKER_HEALTH_URL)/readyz
	@echo "corinto is up; the API on $(API_URL), short links on $(REDIRECT_URL)"
	@echo "health endpoints on $(HEALTH_URL), $(REDIRECT_HEALTH_URL) and $(WORKER_HEALTH_URL)"
	@echo "the demo workspaces, with the key of each when this start seeded them:"
	@$(COMPOSE) exec --no-TTY api /corinto seed --if-missing

# Deletes the demo workspaces and creates them again as the seed has them,
# with new keys, which it prints once. The redirect is started again,
# because it answers from its own copy of the links and would otherwise
# keep the ones that are gone until it reads the whole copy again.
seed:
	$(COMPOSE) exec --no-TTY api /corinto seed
	$(COMPOSE) restart redirect
	@curl --silent --show-error --fail --output /dev/null \
		--retry 30 --retry-delay 1 --retry-all-errors $(REDIRECT_HEALTH_URL)/readyz

# Stops the system and deletes its data. The stack is disposable: the next
# `make up` starts from nothing.
down:
	docker compose --project-name $(COMPOSE_PROJECT) down --volumes

# The database password of the local stack, generated once per clone so that
# no secret is ever versioned. A volume initialised with an earlier password
# is dropped with it, because PostgreSQL would keep the old one.
deploy/.env:
	@docker compose --project-name $(COMPOSE_PROJECT) down --volumes >/dev/null 2>&1 || true
	@umask 077 && printf 'POSTGRES_PASSWORD=%s\n' \
		"$$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')" > $@
	@echo "generated $@ with a new database password"

# Runs the unit tests with the race detector, in random order and without the
# test cache, so that a pass means the tests ran.
test:
	go test -race -shuffle=on -count=1 ./...

# Runs every test, the ones that need a real PostgreSQL included. They start
# one in a container, so Docker must be running. The unit tests run again
# here: a build tag adds files to a package and takes none away.
integration:
	go test -tags integration -race -shuffle=on -count=1 ./...

# Runs every check CI runs, except code scanning, which runs on GitHub. A
# clean clone must pass it.
verify: lint test integration secrets docs image

# Compiles everything and runs the static analysers, over the integration
# tests too.
lint:
	go build ./...
	go vet -tags integration ./...
	$(STATICCHECK) -tags integration ./...
	$(GOLANGCI_LINT) run

# Scans the whole history for secrets. The pre-commit hook looks at what is
# staged; this finds what got past it. The repository is mounted read-only
# and marked safe, because the container runs as another user.
#
# Then it proves that the scan knows the API keys of this project, which
# .gitleaks.toml adds to the scanner's own rules: a key made up on the spot
# and never written anywhere is piped to the scanner and has to be found.
# Exit code 3 means found; anything else fails, a configuration that does
# not load included.
secrets:
	docker run --rm --volume "$(CURDIR):/repo:ro" \
		--env GIT_CONFIG_COUNT=1 \
		--env GIT_CONFIG_KEY_0=safe.directory --env GIT_CONFIG_VALUE_0=/repo \
		$(GITLEAKS_IMAGE) git --redact --no-banner --exit-code 1 /repo
	@printf 'cor_%s_%s\n' "$$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')" \
		"$$(LC_ALL=C tr -dc 'a-z2-7' < /dev/urandom | head -c 52)" | \
	docker run --rm --interactive --volume "$(CURDIR)/.gitleaks.toml:/config.toml:ro" \
		$(GITLEAKS_IMAGE) stdin --config /config.toml --redact --no-banner --exit-code 3 \
		>/dev/null 2>&1; \
	test $$? -eq 3 || { echo "the secret scan does not find an API key of this project" >&2; exit 1; }
	@echo "the secret scan finds an API key of this project"

# Lints every Markdown file with the configuration in .markdownlint-cli2.yaml.
docs:
	docker run --rm --volume "$(CURDIR):/workdir:ro" $(MARKDOWNLINT_IMAGE) "**/*.md"

# Builds the container image. The Go version of the build stage must be the
# one go.mod names: a mismatch fails here instead of surfacing later.
image:
	@test "$$(sed -n 's/^go //p' go.mod)" = \
		"$$(sed -n 's/^FROM .*golang:\([0-9.]*\)-.*/\1/p' deploy/Dockerfile)" || \
		{ echo "go.mod and deploy/Dockerfile name different Go versions" >&2; exit 1; }
	docker build --file deploy/Dockerfile --tag $(IMAGE):dev .
