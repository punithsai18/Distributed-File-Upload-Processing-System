# ── Stage 1: Build the Go binary (frontend/dist is already committed and embedded) ──
FROM golang:1.22-alpine AS go-builder
WORKDIR /app
# Copy all source including vendor/ — no network access required
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -o server .

# ── Stage 2: Minimal runtime image ──────────────────────────────────────────────
FROM alpine:3.20
WORKDIR /app
COPY --from=go-builder /app/server ./server
EXPOSE 8080
CMD ["./server"]
