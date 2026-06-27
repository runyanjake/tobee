pipeline {
  agent any

  environment {
    COMPOSE_FILE  = 'docker-compose.prod.yml'

    // Secret. Set in Jenkins as a "Secret text" credential.
    DISCORD_TOKEN = credentials('tobee-discord-token')

    // Non-secret instance config. Edit here to retarget the deploy.
    // AI_MODEL must support native tool-use (see .claude/DECISIONS.md D-001).
    AI_MODEL          = 'qwen2.5:7b'
    DISCORD_CHANNEL_ID = '1479309607724650649'
  }

  options {
    timestamps()
    disableConcurrentBuilds()
    // Generous: the first deploy pulls the model (several GB) before tobee starts.
    timeout(time: 30, unit: 'MINUTES')
  }

  stages {
    stage('Checkout') {
      steps {
        checkout scm
      }
    }

    stage('Lint') {
      steps {
        // gofmt + go vet run inside the Dockerfile's `lint` target. Using the
        // build context (not a bind mount) keeps this working when Jenkins is
        // containerized and talks to the host Docker daemon — a bind-mounted
        // workspace would resolve against the host and come up empty.
        sh 'docker build --target lint -t tobee-lint .'
      }
    }

    stage('Configure') {
      steps {
        // Render the runtime env file the prod compose reads via env_file.
        // Removed again in post.always so the token never lingers on disk.
        sh '''
          set -eu
          umask 077
          cat > .env.prod <<EOF
AI_PROVIDER_URL=http://ollama:11434
AI_MODEL=${AI_MODEL}
OLLAMA_KEEP_ALIVE=24h
DISCORD_TOKEN=${DISCORD_TOKEN}
DISCORD_CHANNEL_ID=${DISCORD_CHANNEL_ID}
DATA_DIR=data
PROMPTS_DIR=prompts
SESSION_IDLE_TIMEOUT=4h
SESSION_TTL=168h
DEBUG=0
EOF
        '''
      }
    }

    stage('Preflight') {
      steps {
        sh '''
          set -eu
          : "${DISCORD_TOKEN:?DISCORD_TOKEN is empty}" "${AI_MODEL:?AI_MODEL is empty}"

          # Verify the host can actually pass the GPU into a container. Without
          # this Ollama silently runs on CPU and nothing would fail loudly.
          docker run --rm --gpus all ubuntu:22.04 nvidia-smi -L >/dev/null 2>&1 \
            || { echo "GPU passthrough failed: install the Nvidia driver + nvidia-container-toolkit" >&2; exit 1; }

          # .env.prod must already exist here — compose reads it as an env_file.
          docker compose -f "$COMPOSE_FILE" config -q
        '''
      }
    }

    stage('Teardown') {
      steps {
        // Safe: named volumes (ollama-models) and the ./data bind mount are
        // never removed without --volumes, so the pulled model + memory persist.
        sh 'docker compose -f "$COMPOSE_FILE" down --remove-orphans'
      }
    }

    stage('Build & Deploy') {
      steps {
        // Blocks until ollama is healthy and the model pull completes (tobee
        // depends_on service_completed_successfully) before tobee starts.
        sh 'docker compose -f "$COMPOSE_FILE" up -d --build'
      }
    }

    stage('Health Check') {
      steps {
        sh '''
          set -eu

          ocid="$(docker compose -f "$COMPOSE_FILE" ps -q ollama)"
          [ -n "$ocid" ] || { echo "ollama container not found" >&2; exit 1; }
          deadline=$(( $(date +%s) + 180 ))
          while :; do
            health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$ocid")"
            [ "$health" = "healthy" ] && { echo "ollama healthy"; break; }
            [ "$health" = "unhealthy" ] && { echo "ollama reported unhealthy" >&2; exit 1; }
            [ "$(date +%s)" -ge "$deadline" ] && { echo "timed out on ollama (health=$health)" >&2; exit 1; }
            sleep 3
          done

          # tobee has no port or healthcheck; assert the process didn't crash.
          tcid="$(docker compose -f "$COMPOSE_FILE" ps -q tobee)"
          [ -n "$tcid" ] || { echo "tobee container not found" >&2; exit 1; }
          status="$(docker inspect -f '{{.State.Status}}' "$tcid")"
          [ "$status" = "running" ] || { echo "tobee not running (status=$status)" >&2; exit 1; }
        '''
      }
    }

    stage('Smoke Test') {
      steps {
        sh '''
          set -eu
          tcid="$(docker compose -f "$COMPOSE_FILE" ps -q tobee)"
          [ -n "$tcid" ] || { echo "tobee container not found" >&2; exit 1; }

          # cmd/tobee/main.go logs this line only after memory, the LLM client, and the
          # Discord gateway have all initialised — a real end-to-end boot signal.
          deadline=$(( $(date +%s) + 60 ))
          while :; do
            if docker logs "$tcid" 2>&1 | grep -q "tobee is running"; then
              echo "tobee booted"; break
            fi
            st="$(docker inspect -f '{{.State.Status}}' "$tcid")"
            case "$st" in
              exited|dead) echo "tobee exited during boot" >&2; docker logs --tail=50 "$tcid" >&2; exit 1 ;;
            esac
            [ "$(date +%s)" -ge "$deadline" ] && { echo "timed out waiting for tobee boot log" >&2; exit 1; }
            sleep 2
          done
          echo "smoke test passed"
        '''
      }
    }
  }

  post {
    failure {
      sh 'docker compose -f "$COMPOSE_FILE" ps || true'
      sh 'docker compose -f "$COMPOSE_FILE" logs --tail=200 || true'
    }
    cleanup {
      // Runs last, after the failure diagnostics above, so .env.prod is still
      // present when they execute. The secret never outlives the build.
      sh 'rm -f .env.prod'
    }
  }
}
