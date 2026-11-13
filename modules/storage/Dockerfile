# Build from the repository root: docker build -f modules/storage/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/storage-api/ cmd/storage-api/
COPY api/storage/ api/storage/
COPY internal/storage/ internal/storage/
COPY internal/kubeconfig/ internal/kubeconfig/
COPY web/storage/ web/storage/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /diskfleet-api ./cmd/storage-api
RUN mkdir /state

FROM gcr.io/distroless/static:nonroot
COPY --from=build /diskfleet-api /usr/local/bin/diskfleet-api
COPY --chown=10001:10001 --from=build /state /var/lib/diskfleet-api
ENV STATE_DIR=/var/lib/diskfleet-api
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/diskfleet-api"]
