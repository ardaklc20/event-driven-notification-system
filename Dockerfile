# syntax=docker/dockerfile:1

# Build stage (K31): static binaries, no cgo.
FROM golang:1.27.4-trixie AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ \
      ./cmd/api ./cmd/worker ./cmd/migrate ./cmd/mockprovider

# Runtime stage: no shell, no package manager, runs as nonroot (UID 65532).
# There is no ENTRYPOINT; each Compose service picks its program with command.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/api /out/worker /out/migrate /out/mockprovider /app/
EXPOSE 8080 8081
