---
"@agent-gm/cli": patch
---

The OAuth pages carry `Referrer-Policy: same-origin`, so a browser's own enrollment form post no longer arrives as `Origin: null` and is refused; a refused post now names the origin it received and is logged at warn
