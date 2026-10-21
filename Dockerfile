FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ai-model-facade ./cmd/ai-model-facade

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ai-model-facade /ai-model-facade
EXPOSE 8088
# The image has no shell or curl, so the binary checks itself. It reads
# server.listen from the same config path as CMD.
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s CMD ["/ai-model-facade", "health", "/config/config.yaml"]
ENTRYPOINT ["/ai-model-facade"]
CMD ["serve", "/config/config.yaml"]
