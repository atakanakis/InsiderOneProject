# One image, four binaries (api, worker, migrate, mockprovider).
# docker-compose.yml picks the binary of each service with `command`.

FROM golang:1.27-alpine AS build
WORKDIR /src
# Modules in their own layer, so a code change does not download them again.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.22
# wget (busybox) is used by the Compose health checks.
RUN adduser -D -u 10001 app
COPY --from=build /out/ /app/
USER app
EXPOSE 8080 9090
CMD ["/app/api"]
