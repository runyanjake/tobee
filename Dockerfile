FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o tobee ./cmd/tobee

# --- lint (CI-only; not in the runtime build path, so `compose up --build`
# never pays for it). Invoke explicitly: docker build --target lint . ---
FROM builder AS lint
RUN test -z "$(gofmt -l .)" || { echo "gofmt needed:" >&2; gofmt -l . >&2; exit 1; }
RUN go vet ./...

# --- runtime image ---
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/tobee .
COPY prompts ./prompts

# Create mountpoints; data/ is expected to be a bind-mount so the agent's
# memory survives container restarts.
RUN mkdir -p data/memory data/sessions

CMD ["./tobee"]
