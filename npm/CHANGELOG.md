# @agent-gm/cli

## 1.0.5

### Patch Changes

- 5f4ddb8: Add copy buttons to the OAuth enrollment, review, and approval commands, with copy confirmation and manual selection when clipboard access is unavailable.

## 1.0.4

### Patch Changes

- 95ae3dd: Make OAuth enrollment and CLI approval instructions clear, add a simple responsive design, and fix browser security policies so approval polling and the client callback work. Refreshing the completion URL safely displays the request status.

  Fix CLI enrollment and approval scope flags to send JSON arrays as required by the server.

## 1.0.3

### Patch Changes

- 2649e20: The OAuth pages carry `Referrer-Policy: same-origin`, so a browser's own enrollment form post no longer arrives as `Origin: null` and is refused; a refused post now names the origin it received and is logged at warn

## 1.0.2

### Patch Changes

- 42a72e5: `agm auth login --admin` prints a hint when it grants all four scopes; admin sessions are narrowed with `--scopes`
- 7fae2a9: Versions are managed with changesets; the container, binaries and npm package share one version
