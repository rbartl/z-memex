FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
RUN apk add --no-cache git ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o memex ./cmd/server

# The final stage runs nothing, so a foreign-architecture image builds without
# emulation: certificates and zoneinfo come from the builder instead of apk.
FROM alpine:3.24
WORKDIR /app
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /app/memex .
COPY --from=builder /app/migrations ./migrations
COPY --from=builder /app/web/templates ./web/templates
COPY --from=builder /app/web/partials ./web/partials
COPY --from=builder /app/web/static ./web/static
COPY --from=builder /app/LICENSE /app/THIRD_PARTY_NOTICES ./
EXPOSE 3000
CMD ["./memex"]
