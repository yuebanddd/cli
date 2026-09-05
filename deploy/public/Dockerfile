FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /pippit-tool-cli .

FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /pippit-tool-cli /usr/local/bin/pippit-tool-cli
COPY scripts/canvas-command.js scripts/public-canvas-command.js /app/scripts/
COPY release-artifacts/canvas-runtime/ /app/dist/
USER node
ENV PIPPIT_PUBLIC_CANVAS_SCRIPT=/app/scripts/public-canvas-command.js \
    PIPPIT_MEDIA_CACHE_DIR=/var/cache/pippit
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
    CMD curl --fail --silent http://127.0.0.1:8787/readyz || exit 1
ENTRYPOINT ["pippit-tool-cli"]
CMD ["mcp", "serve", "--mode", "public"]
