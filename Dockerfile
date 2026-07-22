FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# CGO for the sqlite driver; distroless base (not static) provides glibc.
RUN CGO_ENABLED=1 go build -ldflags="-s -w -X main.version=${VERSION}" -o /frugal ./cmd/frugal

FROM gcr.io/distroless/base-debian12
COPY --from=build /frugal /frugal
EXPOSE 8080
# Mount a volume at /data and set "data_dir": "/data" for persistence.
ENTRYPOINT ["/frugal"]
