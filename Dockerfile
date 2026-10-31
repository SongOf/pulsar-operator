# The operator binary and the built console in one image.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /pulsar-operator ./cmd/operator

FROM gcr.io/distroless/static:nonroot
COPY --from=build /pulsar-operator /pulsar-operator
COPY --from=web /web/dist /web
USER 65532:65532
ENTRYPOINT ["/pulsar-operator"]
