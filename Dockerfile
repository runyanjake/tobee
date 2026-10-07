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

# --- test (CI-only). Search shells out to grep (D-061), so the suite has to
# run against the grep the runtime image ships, not the BusyBox one this base
# comes with. Invoke explicitly: docker build --target test . ---
FROM builder AS test
# The path is discovered, never assumed: Alpine's grep package does not land in
# /usr/bin, and hardcoding a path failed this build once. What matters is that
# the grep PATH resolves to is GNU, so assert exactly that and print enough on
# failure to see what is actually installed.
RUN apk add --no-cache grep \
 && gnu="$(command -v grep)" \
 && echo "grep on PATH: $gnu" \
 && "$gnu" --version | head -1 \
 && "$gnu" --version | head -1 | grep -qF "GNU grep" \
 || { echo "grep on PATH is not GNU grep:" >&2; command -v grep >&2; apk info -L grep >&2; exit 1; }
RUN TOBEE_GREP_BINS="$(command -v grep)" go test ./...

# --- runtime image ---
FROM alpine:3.20

# Search shells out to grep (D-061). BusyBox provides a grep too and it has no
# -I, so the package alone is not enough: assert at build time that the grep
# PATH resolves to really is GNU. The image is immutable, so an assertion here
# fixes what the process will resolve at runtime without naming a path that
# differs between distributions. GREP_BIN overrides it; boot re-checks either way.
RUN apk add --no-cache ca-certificates tzdata grep \
 && echo "grep on PATH: $(command -v grep)" \
 && grep --version | head -1 \
 && grep --version | head -1 | grep -qF "GNU grep" \
 || { echo "the grep package did not put GNU grep on PATH:" >&2; command -v grep >&2; apk info -L grep >&2; exit 1; }

WORKDIR /app

COPY --from=builder /app/tobee .
COPY prompts ./prompts

# Create mountpoints; data/ is expected to be a bind-mount so memory, jobs,
# and queued tasks survive container restarts.
#
# External stdio MCP servers (MCP_SERVER_<NAME>_COMMAND) run inside this
# image, which has no Node or Python. Use an HTTP server (_URL) or extend
# the image with the runtime the server needs.
RUN mkdir -p data/memory data/tasks data/scheduler/jobs

CMD ["./tobee"]
