# Build from the repository root: docker build -f modules/agent/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/agent/ cmd/agent/
COPY api/storage/ api/storage/
COPY internal/agent/ internal/agent/
COPY internal/kubeconfig/ internal/kubeconfig/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /diskfleet-agent ./cmd/agent

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends lvm2 util-linux udev ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /diskfleet-agent /usr/local/bin/diskfleet-agent
ENTRYPOINT ["/usr/local/bin/diskfleet-agent"]
