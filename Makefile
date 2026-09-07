.PHONY: build check

# The shipped plugin binaries, built into the repo root. gridwell's own
# `make build` invokes this target from $(PLUGINS_DIR) and copies nothing:
# a node finds gridwell-plugin-<kind> by GRIDWELL_PLUGIN_DIR, beside the
# gridwell executable, or on PATH.
# CGO_ENABLED=0 keeps each binary fully static, as the sidecar is.
build:
	cd guest && CGO_ENABLED=0 go build ./...
	cd fs && CGO_ENABLED=0 go build -o ../gridwell-plugin-fs ./cmd/gridwell-plugin-fs
	cd proc && CGO_ENABLED=0 go build -o ../gridwell-plugin-proc ./cmd/gridwell-plugin-proc
	cd gitlab && CGO_ENABLED=0 go build -o ../gridwell-plugin-gitlab ./cmd/gridwell-plugin-gitlab
	cd pages && CGO_ENABLED=0 go build -o ../gridwell-plugin-pages ./cmd/gridwell-plugin-pages
	cd hey && CGO_ENABLED=0 go build -o ../gridwell-plugin-hey ./cmd/gridwell-plugin-hey
	cd gmail && CGO_ENABLED=0 go build -o ../gridwell-plugin-gmail ./cmd/gridwell-plugin-gmail

# The modules that ship on every OS gridwell releases for. proc is not
# here: it reads /proc, so it is a unix plugin by design and the Windows
# release ships without it (gridwell's Makefile drops it from PLUGIN_KINDS).
RELEASE_MODULES := guest fs gitlab pages hey gmail

# check is the per-commit gate: gofmt, then every module vetted and tested
# ALONE (GOWORK=off), so no module can quietly lean on the workspace, then
# the cross-builds for the release targets, then the binaries.
check:
	@bad=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$bad" ]; then echo "gofmt needed (run: gofmt -w <file>):"; echo "$$bad"; exit 1; fi
	@for m in guest fs proc gitlab pages hey gmail; do \
		echo "== module $$m (standalone)"; \
		(cd $$m && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...) || exit 1; \
	done
	# The release builds run on native runners, a tag away, which is far too
	# late to learn that a `//go:build unix` half has a stale other half —
	# guest's host-death probe is one. Cross-build here instead. These run
	# through the WORKSPACE, not standalone, because that is how the release
	# builds resolve guest: the local source, not the last published tag.
	@for m in $(RELEASE_MODULES); do \
		echo "== module $$m (windows + darwin cross-build)"; \
		(cd $$m && GOOS=windows GOARCH=amd64 go build -o /dev/null ./... \
		        && GOOS=darwin GOARCH=arm64 go build -o /dev/null ./...) || exit 1; \
	done
	@echo "== module proc (darwin cross-build; /proc-bound, no windows release)"
	@(cd proc && GOOS=darwin GOARCH=arm64 go build -o /dev/null ./...)
	$(MAKE) build

clean:
	rm -f gridwell-plugin-fs gridwell-plugin-proc gridwell-plugin-gitlab gridwell-plugin-pages gridwell-plugin-hey gridwell-plugin-gmail
