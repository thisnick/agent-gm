# agent-gm — thin wrapper over the devbox scripts (devbox.json is the source of
# truth). Every target must be runnable as `devbox run <script>` too.

DEVBOX ?= devbox run

.PHONY: build fmt lint vet test test-live check tidy clean conformance lint-names no-real-numbers gen-secret

build:      ; $(DEVBOX) build
fmt:        ; $(DEVBOX) fmt
lint:       ; $(DEVBOX) lint
vet:        ; $(DEVBOX) vet
test:       ; $(DEVBOX) test
test-live:  ; $(DEVBOX) test-live
check:      ; $(DEVBOX) check
tidy:       ; $(DEVBOX) tidy
clean:      ; $(DEVBOX) clean
conformance:     ; $(DEVBOX) conformance
lint-names:      ; $(DEVBOX) lint-names
no-real-numbers: ; $(DEVBOX) no-real-numbers
gen-secret:      ; $(DEVBOX) gen-secret
