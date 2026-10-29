---
"@agent-gm/cli": patch
---

The MCP instructions and the `send_message` argument descriptions now warn that a captioned media send on an RCS thread loses its attachment while still reporting `succeeded`, and say to send the text and the file as two calls instead; they also say that a send the phone refuses spends the upload.
