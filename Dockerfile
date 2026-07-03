# syntax=docker/dockerfile:1

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/calsync ./cmd/calsync

# static-debian12 には CA 証明書と tzdata が含まれる(TZ 環境変数がそのまま効く)
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/calsync /calsync
VOLUME /data
ENTRYPOINT ["/calsync", "run"]
CMD ["--config", "/data/calsync.yaml", "--data", "/data"]
