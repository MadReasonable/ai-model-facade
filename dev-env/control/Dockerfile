# control runs Docker Compose, so it needs the Docker CLI; the binary is
# built separately and copied onto the CLI image.
FROM golang:1.25.14-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /control .

FROM docker:29.9.0-cli
COPY --from=build /control /usr/local/bin/control
ENTRYPOINT ["/usr/local/bin/control"]
