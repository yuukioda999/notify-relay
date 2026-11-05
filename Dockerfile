# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/notify-relay ./cmd/notify-relay

# Distroless "nonroot" runs as UID 65532: the service never runs as root (AC-R001-27).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/notify-relay /notify-relay
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/notify-relay"]
