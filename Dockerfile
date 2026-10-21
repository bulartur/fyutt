# One static binary on alpine. The logo and the page are compiled in with
# //go:embed; the only file the container reads is its config.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/fyutt ./cmd/fyutt

FROM alpine:3.21
# ca-certificates to reach api.telegram.org over TLS.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 -h /app fyutt
WORKDIR /app
COPY --from=build /out/fyutt /app/fyutt
USER fyutt
ENV ADDR=:8080 CONFIG_FILE=/app/config/fyutt.json
EXPOSE 8080
# "/" is the only public page, so it is also the health check.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO /dev/null http://127.0.0.1:8080/ || exit 1
ENTRYPOINT ["/app/fyutt"]
