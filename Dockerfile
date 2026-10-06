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
RUN apk add --no-cache grep
RUN /usr/bin/grep --version | head -1 \
  && /usr/bin/grep --version | head -1 | grep -q GNU \
  || { echo "the grep package did not provide GNU grep" >&2; exit 1; }
RUN TOBEE_GREP_BINS=/usr/bin/grep go test ./...

# --- runtime image ---
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata grep

# Search runs this binary (D-061). Named explicitly because BusyBox also
# provides /bin/grep, which has no -I; depending on PATH order between the two
# is a silent way to ship the wrong one. GREP_BIN in .env.prod overrides it.
ENV GREP_BIN=/usr/bin/grep

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
