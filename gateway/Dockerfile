# Gateway image (SPEC-002/3b). Built by `make dev` via infra/compose.
# GO_VERSION has no default: it comes from gateway/go.mod through the Makefile
# (ADR-116: one Go version source), so a build without it fails loudly.
ARG GO_VERSION

FROM golang:${GO_VERSION}-alpine AS build
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src
# Module cache layer: only re-downloaded when go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# Static binary on distroless: no shell, no package manager, non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/gateway"]
