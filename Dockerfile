# Build the gateway (static, cross-arch).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# Runtime: ffmpeg is only needed for the optional `generate` command; the
# serve/strm/thumbs paths are pure Go.
FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates
COPY --from=build /out/gateway /usr/local/bin/gateway
ENV GATEWAY_ADDR=0.0.0.0:8092 \
    GATEWAY_CACHE=/cache \
    GATEWAY_PUBLIC_URL=http://gateway:8092
EXPOSE 8092
ENTRYPOINT ["gateway"]
CMD ["serve"]
