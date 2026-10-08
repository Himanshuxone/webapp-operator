# Syntax docker/dockerfile:1

# Set GO_VERSION to the "go" line in your go.mod.
ARG GO_VERSION=1.26.2
From golang:${GO_VERSION} AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Dependencies first: this layer is cached until go.mod/go.sum change.
COPY go.sum go.mod ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# Static binary (no C library needed).
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd/main.go

# Tiny runtime image: no shell, no package manager.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]