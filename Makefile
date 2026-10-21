VERSION ?= $(shell tr -d ' \n' < VERSION 2>/dev/null || echo 3.0.0)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT_AT ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION := $(VERSION)
COMMIT := $(COMMIT)
BUILT_AT := $(BUILT_AT)
BIN := dist/smartgate-aarch64
HOST_BIN := dist/smartgate-host
VERSION_LDFLAGS := -X smartgate/internal/version.Version=$(VERSION) -X smartgate/internal/version.Commit=$(COMMIT) -X smartgate/internal/version.BuiltAt=$(BUILT_AT)
LDFLAGS := -s -w $(VERSION_LDFLAGS)

GOENV := TMPDIR=$$(pwd)/.tmp GOTMPDIR=$$(pwd)/.tmp GOCACHE=$$(pwd)/.gocache

.PHONY: build build-host frontend-build test archguard fakecore-e2e smoke smoke-locks smoke-worker-control smoke-rescue release clean oss-export \
	secret-scan frontend-check release-gate \
	cutover-token cutover-backup cutover-prepare cutover-preflight cutover-stage cutover-upgrade cutover-verify cutover-rollback cutover-finalize cutover cutover-rehearse collect-evidence

build:
	mkdir -p dist
	mkdir -p .tmp .gocache
	$(GOENV) CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/smartgate

build-host:
	mkdir -p dist
	mkdir -p .tmp .gocache
	$(GOENV) go build -trimpath -ldflags "$(LDFLAGS)" -o $(HOST_BIN) ./cmd/smartgate

frontend-build:
	cd frontend && npm run build

test:
	mkdir -p .tmp .gocache
	$(GOENV) go test ./...
	$(GOENV) go test ./internal/fabric/testdata/m2probe
	python3 -B -m unittest discover -s scripts/cutover -p 'test_*.py'

archguard:
	mkdir -p .tmp .gocache
	$(GOENV) go test -count=1 -run '^TestArchGuard' ./scripts
	$(GOENV) ./scripts/archguard

fakecore-e2e:
	./internal/worker/fakecore/e2e.sh --snapshot

smoke: smoke-locks smoke-worker-control smoke-rescue

smoke-locks: build-host
	./$(HOST_BIN) locks

smoke-worker-control: build-host
	rm -rf .smoke/worker-control
	mkdir -p .smoke/worker-control/config .smoke/worker-control/runtime .smoke/worker-control/etc .smoke/worker-control/tmp
	./$(HOST_BIN) rescue -data-dir "$(PWD)/.smoke/worker-control/etc" -runtime-dir "$(PWD)/.smoke/worker-control/tmp" enter
	worker_ids=$$(awk '/"kind": "worker-service"/ { worker = 1; next } worker && /"id":/ { gsub(/.*"id": "/, ""); gsub(/".*/, ""); print; worker = 0 }' .smoke/worker-control/etc/smartgate-rescue.status); \
		test -n "$$worker_ids"; \
		for worker_id in $$worker_ids; do \
			SMARTGATE_WORKER_CONFIG_DIR="$(PWD)/.smoke/worker-control/config" \
			SMARTGATE_WORKER_RUNTIME_DIR="$(PWD)/.smoke/worker-control/runtime" \
				./scripts/smartgate-worker-control stop "$$worker_id"; \
			test ! -e ".smoke/worker-control/runtime/$$worker_id.pid"; \
		done
	./$(HOST_BIN) rescue -data-dir "$(PWD)/.smoke/worker-control/etc" -runtime-dir "$(PWD)/.smoke/worker-control/tmp" clear

smoke-rescue: build-host
	rm -rf .smoke/rescue
	mkdir -p .smoke/rescue/etc .smoke/rescue/tmp
	./$(HOST_BIN) rescue -data-dir "$(PWD)/.smoke/rescue/etc" -runtime-dir "$(PWD)/.smoke/rescue/tmp" enter
	test -f .smoke/rescue/etc/smartgate-rescue.status
	grep -F '"kind": "worker-service"' .smoke/rescue/etc/smartgate-rescue.status
	grep -F 'smartgate-worker-control' .smoke/rescue/etc/smartgate-rescue.status
	./$(HOST_BIN) rescue -data-dir "$(PWD)/.smoke/rescue/etc" -runtime-dir "$(PWD)/.smoke/rescue/tmp" status
	./$(HOST_BIN) rescue -data-dir "$(PWD)/.smoke/rescue/etc" -runtime-dir "$(PWD)/.smoke/rescue/tmp" clear
	test ! -f .smoke/rescue/etc/smartgate-rescue.status

release: build frontend-build
	rm -rf dist/release
	mkdir -p dist/release/configs/init.d dist/release/scripts dist/release/libexec dist/release/www/smartgate dist/release/docs
	cp $(BIN) dist/release/smartgate
	printf '{\n  "name": "smartgate",\n  "version": "$(VERSION)",\n  "commit": "$(COMMIT)",\n  "built_at": "$(BUILT_AT)",\n  "target": "linux/arm64"\n}\n' > dist/release/version.json
	cp configs/init.d/smartgate dist/release/configs/init.d/smartgate
	cp scripts/install.sh dist/release/scripts/install.sh
	cp scripts/install-fresh.sh dist/release/scripts/install-fresh.sh
	cp scripts/deploy-safe.sh dist/release/scripts/deploy-safe.sh
	cp scripts/deploy-recover.sh dist/release/scripts/deploy-recover.sh
	chmod 0755 dist/release/scripts/install.sh \
		dist/release/scripts/install-fresh.sh \
		dist/release/scripts/deploy-safe.sh \
		dist/release/scripts/deploy-recover.sh
	cp scripts/smartgate-worker-control dist/release/libexec/smartgate-worker-control
	chmod 0755 dist/release/libexec/smartgate-worker-control
	cp -R frontend/dist/. dist/release/www/smartgate/
	cp docs/RELEASE_RUNBOOK.md dist/release/docs/RELEASE_RUNBOOK.md
	cp docs/API_CONTRACT.md dist/release/docs/API_CONTRACT.md
	cp docs/ARCHITECTURE.md dist/release/docs/ARCHITECTURE.md
	cp docs/BUILTIN_RULES.md dist/release/docs/BUILTIN_RULES.md
	cp docs/RULESET_AUTHORING.md dist/release/docs/RULESET_AUTHORING.md
	cp docs/M2_VALIDATION.md dist/release/docs/M2_VALIDATION.md
	cp docs/RELEASE_READINESS.md dist/release/docs/RELEASE_READINESS.md
	cp docs/V2_PARITY.md dist/release/docs/V2_PARITY.md
	cp docs/CUTOVER.md dist/release/docs/CUTOVER.md
	cp docs/CHANGELOG.md dist/release/docs/CHANGELOG.md
	cp LICENSE dist/release/LICENSE
	cp NOTICE dist/release/NOTICE
	cp README.md dist/release/README.md
	cd dist/release && find smartgate configs scripts libexec www version.json LICENSE NOTICE README.md docs -type f -print | sort | xargs sha256sum > SHA256SUMS
	cd dist/release && tar -czf ../smartgate-openwrt-aarch64.tar.gz smartgate configs scripts libexec www version.json SHA256SUMS LICENSE NOTICE README.md docs

# Desensitized source tree for public GitHub (no local caches, no VM material).
# Public snapshots contain no private operational records or development history.
oss-export:
	@git diff --quiet -- . && git diff --cached --quiet -- . || \
		(echo "oss-export: commit tracked changes before exporting" >&2; exit 1)
	rm -rf dist/oss-smartgate
	mkdir -p dist/oss-smartgate
	@set -eu; \
		prefix="$$(git rev-parse --show-prefix)"; tree=HEAD; \
		if [ -n "$$prefix" ]; then tree="HEAD:$${prefix%/}"; fi; \
		git -C "$$(git rev-parse --show-toplevel)" archive --format=tar \
			--output="$$PWD/dist/oss-smartgate-tree.tar" "$$tree"; \
		tar -xf dist/oss-smartgate-tree.tar -C dist/oss-smartgate; \
		rm -f dist/oss-smartgate-tree.tar
	printf '%s\n' '# Open-source export' '' 'This tree was produced by `make oss-export` from commit $(COMMIT).' '' > dist/oss-smartgate/OSS_EXPORT.md
	# belt-and-suspenders: scrub absolute home paths if any slipped in
	@if [ "$$(uname -s)" = Darwin ]; then \
		find dist/oss-smartgate -type f \( -name '*.md' -o -name '*.go' -o -name '*.ts' -o -name '*.tsx' -o -name '*.sh' \) -print0 \
			| xargs -0 sed -i '' -E -e 's|/Users/[A-Za-z0-9_.-]+|/home/user|g'; \
	else \
		find dist/oss-smartgate -type f \( -name '*.md' -o -name '*.go' -o -name '*.ts' -o -name '*.tsx' -o -name '*.sh' \) -print0 \
			| xargs -0 sed -i -E -e 's|/Users/[A-Za-z0-9_.-]+|/home/user|g'; \
	fi
	./scripts/secret-scan.sh dist/oss-smartgate
	cd dist && tar -czf smartgate-oss-source.tar.gz oss-smartgate
	@echo "OSS tree: dist/oss-smartgate"
	@echo "OSS tarball: dist/smartgate-oss-source.tar.gz"

secret-scan:
	./scripts/secret-scan.sh .

frontend-check:
	cd frontend && npm run typecheck && npm run lint && npm test && npm run build

release-gate:
	./scripts/release-gate.sh

# TARGET is required (devvm | router). TOKEN_FILE is optional except where noted.
# FROM_COMMIT   = commit expected to be installed before the upgrade (preflight).
# EXPECT_COMMIT = commit expected to be running after the upgrade (verify);
#                 defaults to the staged artifact's commit.
# The two are distinct so the one-shot `cutover` can carry both.
# cutover-token mints TOKEN_FILE from the admin password (prompted on the
# terminal) when the file does not exist yet, so the one-shot `cutover` needs
# no manual API call beforehand.
cutover-token:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	@test -n "$(TOKEN_FILE)" || (echo "set TOKEN_FILE=<0600 path for the admin Bearer token>" >&2; exit 2)
	./scripts/cutover/05-token.sh --token-file "$(TOKEN_FILE)" --if-missing

cutover-preflight:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	./scripts/cutover/00-preflight.sh $(if $(FROM_COMMIT),--expect-commit $(FROM_COMMIT),) $(if $(TOKEN_FILE),--token-file $(TOKEN_FILE),)

cutover-backup:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	bash ./scripts/cutover/08-backup.sh --output "$(BACKUP_DIR)" --expect-commit "$(FROM_COMMIT)"

cutover-prepare:
	bash ./scripts/cutover/09-prepare.sh

cutover-stage:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	./scripts/cutover/10-stage.sh $(if $(TARBALL),--tarball $(TARBALL),) $(if $(ALLOW_UNGATED),--allow-ungated,) $(if $(filter 1,$(EMERGENCY_HOTFIX)),--emergency-hotfix,)

cutover-upgrade:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	./scripts/cutover/20-upgrade.sh $(if $(TOKEN_FILE),--token-file $(TOKEN_FILE),) $(if $(filter 1,$(EMERGENCY_HOTFIX)),--emergency-hotfix,)

cutover-verify:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	./scripts/cutover/30-verify.sh --token-file "$(TOKEN_FILE)" $(if $(EXPECT_COMMIT),--expect-commit $(EXPECT_COMMIT),) $(if $(SLUG),--slug $(SLUG),)

cutover-rollback:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	./scripts/cutover/40-rollback.sh $(or $(ROLLBACK_MODE),--list)

cutover-finalize:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	./scripts/cutover/90-finalize.sh $(if $(TARBALL),--tarball $(TARBALL),)

.NOTPARALLEL: cutover
cutover: $(if $(TOKEN_FILE),cutover-token,) $(if $(BACKUP_DIR),cutover-backup,) $(if $(PREPARE_SPACE),cutover-prepare,) cutover-preflight cutover-stage cutover-upgrade cutover-verify cutover-finalize

cutover-rehearse:
	@test "$(TARGET)" = devvm || (echo "cutover-rehearse is rehearsal-only (TARGET=devvm)" >&2; exit 2)
	$(MAKE) cutover-preflight TARGET=devvm $(if $(TOKEN_FILE),TOKEN_FILE=$(TOKEN_FILE),)
	$(MAKE) cutover-stage TARGET=devvm ALLOW_UNGATED=$(ALLOW_UNGATED)
	$(MAKE) cutover-upgrade TARGET=devvm $(if $(TOKEN_FILE),TOKEN_FILE=$(TOKEN_FILE),)
	@echo "cutover-rehearse: install finished; run cutover-verify after auth setup, then 40-rollback.sh"

collect-evidence:
	@test -n "$(TARGET)" || (echo "set TARGET=devvm or TARGET=router" >&2; exit 2)
	@test -n "$(SLUG)" || (echo "set SLUG=<name>" >&2; exit 2)
	./scripts/cutover/collect-evidence.sh "$(SLUG)" $(if $(TOKEN_FILE),--token-file $(TOKEN_FILE),)

clean:
	rm -rf dist .tmp .gocache .smoke
