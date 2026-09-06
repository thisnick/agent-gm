# Fixtures for the pinned upstream commit

Fixtures live under `testdata/libgm/<commit>/`, named for the pinned
mautrix-gmessages commit, so a pin bump cannot silently reuse a fixture that
described the previous tree (spec section 13.4).

They are **source-derived, not live captures**: every identifier is a fixture
label or a fictional `555` number, and every key is a nonfunctional
placeholder. No real phone number, message body, token, Google cookie or
anything from a session file goes in one.

Slice 1 adds none. The twenty assertions of spec section 13.4 read the pinned
upstream tree directly -- `devbox run fixture-validation` clones
mautrix-gmessages at `be48a58` and runs `internal/upstream` against it -- so
there is nothing here yet for them to consume. Later slices that need a
recorded `gmproto` payload put it here.
