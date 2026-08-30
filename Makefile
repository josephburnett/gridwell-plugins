.PHONY: build check

# The three shipped plugin binaries, built into the repo root. gridwell's own
# `make build` invokes this target from $(PLUGINS_DIR) and copies nothing:
# a node finds gridwell-plugin-<kind> by GRIDWELL_PLUGIN_DIR, beside the
# gridwell executable, or on PATH.
# CGO_ENABLED=0 keeps each binary fully static, as the sidecar is.
build:
	cd guest && CGO_ENABLED=0 go build ./...
	cd fs && CGO_ENABLED=0 go build -o ../gridwell-plugin-fs ./cmd/gridwell-plugin-fs
	cd proc && CGO_ENABLED=0 go build -o ../gridwell-plugin-proc ./cmd/gridwell-plugin-proc
	cd gitlab && CGO_ENABLED=0 go build -o ../gridwell-plugin-gitlab ./cmd/gridwell-plugin-gitlab

# check is the per-commit gate: gofmt, then every module vetted and tested
# ALONE (GOWORK=off), so no module can quietly lean on the workspace, and
# finally the binaries.
check:
	@bad=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$bad" ]; then echo "gofmt needed (run: gofmt -w <file>):"; echo "$$bad"; exit 1; fi
	@for m in guest fs proc gitlab; do \
		echo "== module $$m (standalone)"; \
		(cd $$m && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./...) || exit 1; \
	done
	$(MAKE) build

clean:
	rm -f gridwell-plugin-fs gridwell-plugin-proc gridwell-plugin-gitlab
