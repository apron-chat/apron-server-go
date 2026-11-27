.PHONY: install dev-web dev-server check vuln test test-go test-wire test-conformance test-interop test-perf bench bench-smoke bench-compare profile build build-web serve run

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
	cd apron && go mod download

dev-web:
	npm --prefix $(WEB) run dev -- --host 127.0.0.1 --port 5173 --strictPort

dev-server:
	go run ./cmd/aprond

# staticcheck is a tool of the root module (go.mod), at the version it pins; staticcheck.conf configures it.
check:
	go vet ./... ./apron/...
	go tool staticcheck ./... ./apron/...

# Known vulnerabilities that the code reaches, in its modules or the Go release that builds it. govulncheck runs on
# that Go release (go.work's toolchain), as an older one cannot read its standard library.
vuln:
	GOTOOLCHAIN=$$(go env GOVERSION) go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./... ./apron/...

test: test-go test-wire

test-go:
	go test -race ./... ./apron/...

# Go benchmarks (PROFILING.md). BENCH is a -bench pattern, BENCH_PKGS the packages, BENCH_COUNT the runs of each (6
# or more for benchstat), BENCH_FLAGS more go test flags, such as -cpu=1,2,4. Results go to build/bench/head.txt.
BENCH ?= .
BENCH_PKGS ?= ./... ./apron/...
BENCH_COUNT ?= 6
BENCH_FLAGS ?=
bench:
	mkdir -p build/bench
	go test -run '^$$' -bench '$(BENCH)' -benchmem -count $(BENCH_COUNT) $(BENCH_FLAGS) $(BENCH_PKGS) | tee build/bench/head.txt

# Every benchmark once, so that they keep building and running (CI). -short skips the largest cases, such as a room of
# 5,000 members, whose setup alone takes minutes.
bench-smoke:
	go test -short -run '^$$' -bench . -benchtime 1x $(BENCH_PKGS) > /dev/null

# The same benchmarks on BASE (origin/main by default) and on the working tree, compared with benchstat.
BASE ?= origin/main
bench-compare:
	BASE='$(BASE)' BENCH='$(BENCH)' PKGS='$(BENCH_PKGS)' COUNT='$(BENCH_COUNT)' BENCHFLAGS='$(BENCH_FLAGS)' \
		scripts/bench-compare.sh

# CPU, allocation, mutex, and block profiles of the benchmarks BENCH matches in one package PKG, in build/profile,
# with the top of each. Read one further with go tool pprof, such as -list or -http.
PKG ?= ./apron/server
profile:
	mkdir -p build/profile
	go test -run '^$$' -bench '$(BENCH)' -benchmem $(BENCH_FLAGS) -o $(CURDIR)/build/profile/bench.test -outputdir $(CURDIR)/build/profile \
		-cpuprofile cpu.pprof -memprofile mem.pprof -mutexprofile mutex.pprof -blockprofile block.pprof $(PKG)
	go tool pprof -top -nodecount=20 build/profile/cpu.pprof
	go tool pprof -top -nodecount=20 -sample_index=alloc_space build/profile/mem.pprof
	go tool pprof -top -nodecount=10 build/profile/mutex.pprof
	go tool pprof -top -nodecount=10 build/profile/block.pprof

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
