---
"@agent-gm/cli": patch
---

Run CI on pull requests and on pushes to `main` and to version tags only. A push to any other branch started a second run of every job beside the pull request's own — the same commit, the same verdict, twice the runners — so the branch push trigger is gone and the pull request run is the one that counts. `main` keeps its push run because `version.yml` waits on it before cutting a tag and `release.yml` requires it green before publishing, and a `v*` tag keeps its run because that is what builds the image `release.yml` waits for.
