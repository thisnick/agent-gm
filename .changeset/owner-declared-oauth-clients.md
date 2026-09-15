---
"@agent-gm/cli": minor
---

Add owner-declared OAuth clients, for connectors that cannot register themselves. `POST /v1/admin/clients` and `agm admin clients create <client-id> --redirect <uri>` declare a public client whose id the owner chooses, which is what a connector that asks its operator to paste a client_id needs; an id beginning client_ is refused, since that prefix is what /oauth/register mints. Such a client may carry --default-resource, letting it omit `resource` at /oauth/authorize, read as this server's one canonical resource. Nothing else is relaxed: the redirect rules, PKCE, the enrollment code and the owner's approval all apply unchanged, and creation writes a client.created audit row. Migration 0009 adds the two columns; existing registrations read back unchanged.
