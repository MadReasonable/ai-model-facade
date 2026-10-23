# The build stage runs on the build machine and cross-compiles, so a
# multi-platform build doesn't run the Go compiler under emulation.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /ai-model-facade ./cmd/ai-model-facade

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ai-model-facade /ai-model-facade
EXPOSE 8088
# The image has no shell or curl, so the binary checks itself. It reads
# server.listen from the same config path as CMD.
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s CMD ["/ai-model-facade", "health", "/config/config.yaml"]
ENTRYPOINT ["/ai-model-facade"]
CMD ["serve", "/config/config.yaml"]
