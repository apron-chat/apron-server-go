.PHONY: install dev-web dev-server check test test-go test-wire test-interop test-perf build build-web serve run

# The web client lives in apron-chat/apron-web. The browser tests, dev-web, and
# run use a checkout of it here; replace it with a symlink to use your own.
WEB := .apron-web

# The shared protocol fixtures come from shazow/apron, pinned as a submodule.
FIXTURES := testdata/apron/tests/fixtures

$(WEB):
	git clone --depth 1 https://github.com/apron-chat/apron-web.git $@

$(FIXTURES):
	git submodule update --init --depth 1 testdata/apron

install: $(WEB) $(FIXTURES)
	npm --prefix $(WEB) ci
	npm --prefix tests/interop ci
	go mod download

dev-web:
	npm --prefix $(WEB) run dev -- --host 127.0.0.1 --port 5173 --strictPort

dev-server:
	go run ./cmd/aprond

check:
	go vet ./...

test: test-go test-wire

test-go:
	go test -race ./...

# Rendering benchmarks on the production build; fails when a count rises above perf-ceilings.json.
test-perf: build-web
	cd tests/interop && npx playwright test --config=perf.config.ts

test-interop:
	npm --prefix tests/interop test

test-wire: $(FIXTURES)
	npm --prefix tests/interop run test:wire

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
