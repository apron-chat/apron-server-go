.PHONY: install dev-web dev-server check test test-go test-wire test-conformance test-interop test-perf build build-web serve run

# The web client lives in apron-chat/apron-web, pinned as a submodule. The
# browser tests, dev-web, and run use it; check out another commit in it to
# test against that version.
WEB := .apron-web

# The shared protocol fixtures come from shazow/apron, pinned as a submodule.
FIXTURES := testdata/apron/tests/fixtures

$(WEB)/package.json:
	git submodule update --init --depth 1 $(WEB)

$(FIXTURES):
	git submodule update --init --depth 1 testdata/apron

install: $(WEB)/package.json $(FIXTURES)
	npm --prefix $(WEB) ci
	npm --prefix tests/interop ci
	go mod download
	cd wire && go mod download

dev-web:
	npm --prefix $(WEB) run dev -- --host 127.0.0.1 --port 5173 --strictPort

dev-server:
	go run ./cmd/aprond

check:
	go vet ./... ./wire/...

test: test-go test-wire

test-go:
	go test -race ./... ./wire/...

# Rendering benchmarks on the production build; fails when a count rises above perf-ceilings.json.
test-perf: build-web
	cd tests/interop && npx playwright test --config=perf.config.ts

test-interop:
	npm --prefix tests/interop test

test-wire: $(FIXTURES)
	npm --prefix tests/interop run test:wire

# The protocol's conformance checks (testdata/apron/tests/conformance), as testdata/apron's DEVELOPMENT.md runs them
# in an implementation's CI: aprond with an in-memory store, its own origin a passkey origin, and email sign-in that
# logs its mail, in a Python environment of their own under build/. The passkey portal signs in on aprond's pages in
# Chromium, with tests/interop's Playwright (`npx playwright install chromium` there). A portal that cannot run skips
# the sign-in checks rather than failing them, so such a skip fails the target.
CONFORM_PORT := 8080
test-conformance: $(FIXTURES)
	go build -o build/aprond ./cmd/aprond
	test -x build/conform/bin/python || python3 -m venv build/conform
	build/conform/bin/pip install -q "websockets>=14,<17" "jsonschema>=4.18" "cryptography>=42"
	./build/aprond --addr=127.0.0.1:$(CONFORM_PORT) --store=memory --upload.dir="$$(mktemp -d)" \
		--webauthn.origin=http://localhost:$(CONFORM_PORT) --email.enable --email.sender=log & pid=$$!; \
		trap 'kill $$pid; wait $$pid' EXIT; \
		NODE_PATH="$(CURDIR)/tests/interop/node_modules$${NODE_PATH:+:$$NODE_PATH}" \
			build/conform/bin/python testdata/apron/tests/conformance/conform.py ws://localhost:$(CONFORM_PORT)/ws \
			--portal "node testdata/apron/tests/conformance/portals/passkey.cjs" --json build/conformance.json && \
		build/conform/bin/python -c 'import json, sys; skips = [r["id"] for r in json.load(open("build/conformance.json"))["results"] if r["status"] == "SKIP" and "portal" in r["message"]]; skips and sys.exit("the passkey portal could not sign in, so these checks skipped: " + ", ".join(skips))'

# aprond serves this build itself (serve, run, test-perf), so it connects to
# its own origin's /ws and loads media from it: the web client's
# .env.production otherwise points builds at wss://server.apron.chat/.
build-web:
	VITE_DEFAULT_SERVER_URL= VITE_TRUSTED_MEDIA_ORIGINS= npm --prefix $(WEB) run build

build: build-web
	go build -o build/aprond ./cmd/aprond

serve:
	./build/aprond --static-dir $(WEB)/build

run: build
	$(MAKE) serve
