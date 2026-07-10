FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO for the sqlite driver; distroless base (not static) provides glibc.
RUN CGO_ENABLED=1 go build -ldflags="-s -w" -o /awsobs ./cmd/awsobs

FROM gcr.io/distroless/base-debian12
COPY --from=build /awsobs /awsobs
EXPOSE 8080
# Mount a volume at /data and set "data_dir": "/data" for persistence.
ENTRYPOINT ["/awsobs"]
