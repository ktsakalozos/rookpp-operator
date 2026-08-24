# Build
FROM golang:1.25 AS build
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/ internal/
COPY cmd/ cmd/
ARG TARGET=cmd
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/manager ./cmd
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/agent ./cmd/agent

# Manager image (distroless)
FROM gcr.io/distroless/static:nonroot AS manager
WORKDIR /
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]

# Agent image (needs util-linux for lsblk)
FROM debian:bookworm-slim AS agent
RUN apt-get update && apt-get install -y --no-install-recommends util-linux && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/agent /agent
ENTRYPOINT ["/agent"]
