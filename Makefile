# The single entry point of the repository (ADR 0015). CI runs these same
# targets, so what passes here passes there.

# Command-line tools that CI and local runs share, pinned in this one place
# (docs/engineering-rules.md, "Environment and lockfiles"). Container images
# carry their digest; Go tools are built by the toolchain of go.mod at the
# version named here. Update one tool at a time.
GITLEAKS_IMAGE := ghcr.io/gitleaks/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f
MARKDOWNLINT_IMAGE := davidanson/markdownlint-cli2:v0.24.0@sha256:70ead7c0352d22d04bbe612cd96b54ec591538d8f4626cafe391bb2ecf3639ca
K6_IMAGE := grafana/k6:2.4.0@sha256:d41bb79cd8de5a7ad619b53570e3712633e8bff0937010a5c6c57a4747e7a276
BUILDKIT_IMAGE := moby/buildkit:v0.35.0@sha256:aa632b921ef07411d2cb77292cd34213d4f7b384001d6fceef1e1f25bf9752bb
STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
OASDIFF := go run github.com/oasdiff/oasdiff@v1.37.0

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

.PHONY: up down seed test integration verify lint contract secrets docs image image-go-version \
	release-image bench bench-smoke bench-break bench-replica

# Starts the whole system on this machine and waits until it reports ready.
# The first start seeds two demo workspaces, with a history of ninety days
# that takes some seconds to write, and prints a key for each, this once:
# only a hash of a key is stored. A later start leaves them as they are and
# prints no key.
up: deploy/.env
	$(COMPOSE) up --build --detach --wait
	@curl --silent --show-error --fail --output /dev/null \
		--retry 30 --retry-delay 1 --retry-all-errors $(HEALTH_URL)/readyz
	@curl --silent --show-error --fail --output /dev/null \
		--retry 30 --retry-delay 1 --retry-all-errors $(REDIRECT_HEALTH_URL)/readyz
	@curl --silent --show-error --fail --output /dev/null \
		--retry 30 --retry-delay 1 --retry-all-errors $(WORKER_HEALTH_URL)/readyz
	@echo "corinto is up; the panel and the API on $(API_URL), short links on $(REDIRECT_URL)"
	@echo "health endpoints on $(HEALTH_URL), $(REDIRECT_HEALTH_URL) and $(WORKER_HEALTH_URL)"
	@echo "the demo workspaces, with the key of each when this start seeded them:"
	@$(COMPOSE) exec --no-TTY api /corinto seed --if-missing

# Deletes the demo workspaces and creates them again as the seed has them,
# with new keys, which it prints once, and with their history, which takes
# some seconds to write. The redirect is started again, because it answers
# from its own copy of the links and would otherwise keep the ones that are
# gone until it reads the whole copy again. While it starts it does not
# answer: it is asked every second, and only a redirect that is still not
# ready after half a minute is said to have failed.
seed:
	$(COMPOSE) exec --no-TTY api /corinto seed
	$(COMPOSE) restart redirect
	@tries=0; \
	until curl --silent --fail --output /dev/null $(REDIRECT_HEALTH_URL)/readyz; do \
		tries=$$((tries + 1)); \
		if [ "$$tries" -ge 30 ]; then \
			echo "redirect did not report ready after it was started again" >&2; \
			exit 1; \
		fi; \
		sleep 1; \
	done

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
verify: lint contract test integration secrets docs image bench-smoke

# Compiles everything and runs the static analysers, over the integration
# tests too.
lint:
	go build ./...
	go vet -tags integration ./...
	$(STATICCHECK) -tags integration ./...
	$(GOLANGCI_LINT) run

# Compares the contract of the API with the one of the last release, and
# fails on a breaking change that contract/CHANGELOG.md does not announce,
# and while the entries of that release are still under `Unreleased` there.
# It needs the history with its tags; before the first release there is
# nothing to compare with.
contract:
	OASDIFF="$(OASDIFF)" contract/breaking.sh

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
image: image-go-version
	docker build --file deploy/Dockerfile --tag $(IMAGE):dev .

image-go-version:
	@test "$$(sed -n 's/^go //p' go.mod)" = \
		"$$(sed -n 's/^FROM .*golang:\([0-9.]*\)-.*/\1/p' deploy/Dockerfile)" || \
		{ echo "go.mod and deploy/Dockerfile name different Go versions" >&2; exit 1; }

# Builds the image of a release for amd64 and arm64 and pushes it under the
# tag of the release. The release workflow runs it; by hand it needs a
# session in the registry. The builder Docker brings keeps one architecture
# under a name, so the build has a builder of its own, at a pinned version,
# which is removed when the build ends, however it ends.
release-image: image-go-version
	@test -n "$(VERSION)" || \
		{ echo "name the release: make release-image VERSION=v1.2.3" >&2; exit 1; }
	docker buildx create --name corinto-release --driver docker-container \
		--driver-opt image=$(BUILDKIT_IMAGE)
	docker buildx build --builder corinto-release --file deploy/Dockerfile \
		--platform linux/amd64,linux/arm64 \
		--label org.opencontainers.image.source=https://github.com/majimtor/corinto \
		--label org.opencontainers.image.version=$(VERSION) \
		--label org.opencontainers.image.revision=$$(git rev-parse HEAD) \
		--tag $(IMAGE):$(VERSION) --push .; \
	built=$$?; docker buildx rm corinto-release; exit $$built

# Runs the load scenario of PLAN.md section 15 against a stack of its own,
# with the limits of the plan, and fails when an objective is missed: the
# burst, ten minutes of it. It is what the figures of docs/benchmarks.md
# are reproduced with, on a machine with nothing else to do (ADR 0020).
bench: deploy/.env
	K6_IMAGE=$(K6_IMAGE) bench/run.sh burst

# The same scenario for a few seconds, with thresholds ten times as wide.
# CI runs it: it says that the command works and that nothing got worse by
# an order of magnitude, and it publishes nothing.
bench-smoke: deploy/.env
	K6_IMAGE=$(K6_IMAGE) bench/run.sh smoke

# Raises the rate until the service time misses its objective, and says
# where. Run by hand, for the figure the plan asks for.
bench-break: deploy/.env
	K6_IMAGE=$(K6_IMAGE) bench/run.sh break

# Measures, five times, the memory the copy of the links takes in a
# process: what it holds, and the most the process holds from the system
# while the whole copy is read again. It needs Go and nothing else, and it
# is what the figures of the replica in docs/benchmarks.md are reproduced
# with.
bench-replica:
	go test -run '^$$' -bench Memory -benchtime 1x -count 5 ./internal/resolve/
