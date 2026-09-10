---
"@agent-gm/cli": patch
---

Cache Go's build and module caches, and golangci-lint's, between CI runs. Every job started with them empty, so each one recompiled the standard library three times over — linux, windows and darwin `vet` — and every dependency again race-instrumented, which is where `check`'s five and a half minutes went. Measured locally: 133s cold, 42s warm, 45s warm with Agent GM's own code changed, so the key is the `go.sum`/`devbox.lock` hash rather than the commit, and only the `check` job saves the entry the others read.
