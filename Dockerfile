FROM golang:1.26-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o memex ./cmd/server

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/memex .
COPY --from=builder /app/migrations ./migrations
COPY --from=builder /app/web/templates ./web/templates
COPY --from=builder /app/web/partials ./web/partials
COPY --from=builder /app/web/static ./web/static
COPY --from=builder /app/LICENSE /app/THIRD_PARTY_NOTICES ./
EXPOSE 3000
CMD ["./memex"]
