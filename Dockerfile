# syntax=docker/dockerfile:1

# Build stage (K31): static binaries, no cgo.
FROM golang:1.27.4-trixie AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# Runtime stage: no shell, no package manager, runs as nonroot (UID 65532).
# There is no ENTRYPOINT; each Compose service picks its program with command.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/api /out/migrate /app/
EXPOSE 8080
