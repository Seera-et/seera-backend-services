# Build stage.
FROM golang:1.26 AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off and a static binary, because the runtime image has no libc.
# Migrations are embedded in the binary, so nothing else needs copying.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# Runtime stage: no shell, no package manager, non-root.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /api

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/api"]
