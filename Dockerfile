# syntax=docker/dockerfile:1
FROM golang:1.24-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends libx264-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM debian:bookworm-slim
# ffmpeg (decode + Opus) and the x264 shared library the server links against.
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg libx264-164 fonts-dejavu-core ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/server /app/server
COPY scripts /app/scripts
# Generate the stand-in at build time so the image works out of the box;
# mount your own file over /app/media or set PUFFER_VIDEO.
RUN ./scripts/make-standin.sh media/standin.mp4 >/dev/null 2>&1
EXPOSE 8080/tcp 50000/udp
ENTRYPOINT ["/app/server"]
