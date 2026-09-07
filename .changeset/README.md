# Changesets

This directory holds the pending changes for the next release. **Every version
Agent GM ships comes from here** — the container image, the `agent-gm` server,
the `agm` command line and `@agent-gm/cli` on npm all carry the version in
`npm/package.json`, and this is the only thing that bumps it (decision D39,
spec §14.3).

A pull request that changes anything but documentation carries a changeset:

```bash
devbox run changeset          # pick patch/minor/major, write one sentence
git add .changeset && git commit
```

CI refuses a code pull request that has no changeset unless it is labelled
`no-release`. A documentation-only pull request needs none.

On merge to `main`, `.github/workflows/version.yml` opens or updates a single
**Version Packages** pull request that bumps `npm/package.json` and prepends
the entries to `CHANGELOG.md`. Merging *that* pull request is what cuts the
release: the tag, the image, the archives and the npm publish all follow from
the version it committed.

Release candidates: `npx changeset pre enter rc` … `npx changeset pre exit`.
See `docs/operations.md`, "Cutting a release".
