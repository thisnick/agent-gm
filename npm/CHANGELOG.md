# @agent-gm/cli

## 1.0.3

### Patch Changes

- 2649e20: The OAuth pages carry `Referrer-Policy: same-origin`, so a browser's own enrollment form post no longer arrives as `Origin: null` and is refused; a refused post now names the origin it received and is logged at warn

## 1.0.2

### Patch Changes

- 42a72e5: `agm auth login --admin` prints a hint when it grants all four scopes; admin sessions are narrowed with `--scopes`
- 7fae2a9: Versions are managed with changesets; the container, binaries and npm package share one version
