# Agent GM -- spec section 14.1.
#
# Two stages. The build stage is `golang:1.27`, the pinned Go version; the
# runtime is `gcr.io/distroless/static-debian12:nonroot`, which is possible
# only because CGO_ENABLED=0 and the SQLite driver is pure Go
# (modernc.org/sqlite): there is nothing to link against, so there is no libc
# in the image, no shell, no package manager and no curl.
#
# That is also why the healthcheck is a subcommand of the program itself
# (`agent-gm healthcheck`) rather than a `curl` one-liner: in an image with no
# shell, the only thing a HEALTHCHECK can run is a binary, and the only binary
# here is this one.

FROM --platform=${BUILDPLATFORM} golang:1.27 AS build
WORKDIR /src

# go.mod and go.sum first, so the module download layer is reused whenever the
# dependencies have not changed. The libgm pin (spec section 3.6) is a fact of
# go.mod, so this layer's cache key is exactly the pin.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is the release version and COMMIT is the git commit being built.
# Both are passed by CI (scripts/image.sh). They are ARGs rather than
# something the build discovers because the build context has no .git: a
# container build cannot read a VCS stamp, so without these the binary would
# report commit "unknown" -- and section 1.4 makes the commit an AGPL section
# 13 obligation, since GET /v1/health and the MCP serverInfo offer the
# corresponding source at the exact built commit.
ARG VERSION=dev
ARG COMMIT=unknown

# TARGETOS/TARGETARCH are set by buildx for each platform of the manifest
# list, so one `docker buildx build --platform linux/amd64,linux/arm64` cross
# compiles both without QEMU in this stage. CGO_ENABLED=0 makes that possible
# and makes the distroless static base possible.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/agent-gm ./cmd/agent-gm

# The data directory is created HERE, owned by the runtime user, and copied
# into the final image. Distroless ships no /data, and a bind mount or a
# named volume onto a path that does not exist in the image is created by the
# daemon as root:root -- which a USER nonroot process cannot write, so the
# server would fail to open its database on the very first start. Creating it
# in the image with the right ownership is what makes
# `docker compose -f compose.example.yml up` work with nothing but the three
# required variables.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/agent-gm /usr/local/bin/agent-gm
# 65532 is `nonroot` in the distroless base; the numeric form is used so the
# ownership is right regardless of how the daemon resolves names.
COPY --from=build --chown=65532:65532 /out/data /data

ENV AGENT_GM_DATA_DIR=/data AGENT_GM_LISTEN_ADDR=0.0.0.0:8080
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot

# `agent-gm healthcheck` GETs /healthz against AGENT_GM_LISTEN_ADDR and exits
# 0 only on 200 (spec sections 7.5, 14.1). The exec form is required: there is
# no shell to parse the string form.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/agent-gm", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/agent-gm"]
CMD ["serve"]
