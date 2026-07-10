# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates wget \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /app

# Compose / local: docker build --target api|worker
FROM runtime AS api
COPY --from=build /out/api /usr/local/bin/service
USER nobody
CMD ["service"]

FROM runtime AS worker
COPY --from=build /out/worker /usr/local/bin/service
USER nobody
CMD ["service"]

# Default stage (DigitalOcean): both binaries; select via run_command (api|worker).
FROM runtime AS both
COPY --from=build /out/api /usr/local/bin/api
COPY --from=build /out/worker /usr/local/bin/worker
USER nobody
CMD ["api"]
