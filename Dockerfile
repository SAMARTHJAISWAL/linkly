# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src

# Download modules in their own layer so code changes don't refetch them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# modernc.org/sqlite is pure Go, so CGO can stay off and the binary is static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/linkly ./cmd/linkly

# The runtime image has no shell, so create the data directory here,
# owned by distroless's nonroot user (65532).
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# ---- runtime --------------------------------------------------------------
# distroless/static: CA certificates, tzdata and /etc/passwd, nothing else.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/linkly /usr/local/bin/linkly
COPY --from=build --chown=65532:65532 /out/data /data

ENV LINKLY_ADDR=:8080 \
    LINKLY_DB=/data/linkly.db

USER nonroot:nonroot
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/linkly", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/linkly"]
