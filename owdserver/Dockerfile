# syntax=docker/dockerfile:1
# Standalone image for the one-way delay server; build from this directory:
#   docker build -t owdserver owdserver/
#   docker run --network host owdserver          (UDP and TCP 4433)
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/owdserver .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/owdserver /owdserver
EXPOSE 4433/udp 4433/tcp
ENTRYPOINT ["/owdserver"]
