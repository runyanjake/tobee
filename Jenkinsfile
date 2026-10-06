pipeline {
  agent any

  environment {
    COMPOSE_FILE  = 'docker-compose.prod.yml'

    // Secret. Set in Jenkins as a "Secret text" credential.
    DISCORD_TOKEN = credentials('tobee-discord-token')

    // Runtime log level. Stored as a Secret text credential so it can be
    // dialled up to debug from the Jenkins UI without editing this file.
    // Accepts debug | info | warn | error.
    LOG_LEVEL = credentials('tobee-log-level')

    // Discord webhook for post-build notifications. Stored as a Secret
    // text credential. Consumed by discordSend in post.always.
    DISCORD_WEBHOOK = credentials('discord-pws-builds-channel-webhook')

    // Non-secret instance config. Edit here to retarget the deploy.
    // AI_MODEL: any instruction-tuned Ollama model; output is schema-constrained
    // (see .claude/DESIGN.md D-041).
    // To move off the bundled Ollama, point AI_PROVIDER_URL at another
    // OpenAI-compatible server and add AI_API_KEY as a Secret text credential.
    AI_PROVIDER_URL    = 'http://ollama:11434'
    AI_MODEL           = 'qwen2.5:7b'
    DISCORD_CHANNEL_ID = '1479309607724650649'
    // Wall-clock zone for reminders and <context>. Without it the container
    // runs on UTC and the agent schedules against the wrong clock (D-049).
    TZ                 = 'America/Los_Angeles'
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

    stage('Test') {
      steps {
        // Runs go test against the GNU grep the runtime image installs, so a
        // grep variant that behaves differently fails here and not in a turn
        // (D-061). Same build-context reasoning as Lint.
        sh 'docker build --target test -t tobee-test .'
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
AI_PROVIDER_URL=${AI_PROVIDER_URL}
AI_MODEL=${AI_MODEL}
OLLAMA_KEEP_ALIVE=24h
DISCORD_TOKEN=${DISCORD_TOKEN}
DISCORD_CHANNEL_ID=${DISCORD_CHANNEL_ID}
TZ=${TZ}
DATA_DIR=data
PROMPTS_DIR=prompts
LOG_LEVEL=${LOG_LEVEL}
EOF
        '''
      }
    }

    stage('Preflight') {
      steps {
        sh '''
          set -eu
          : "${DISCORD_TOKEN:?DISCORD_TOKEN is empty}" "${AI_MODEL:?AI_MODEL is empty}" "${LOG_LEVEL:?LOG_LEVEL is empty}"

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

          # "tobee is running" means every MCP server connected and every source
          # started. Sources retry on failure instead of exiting, so a bad token
          # would still boot: "discord: connected" is the end-to-end signal that
          # the gateway actually authenticated.
          deadline=$(( $(date +%s) + 60 ))
          while :; do
            logs="$(docker logs "$tcid" 2>&1)"
            if echo "$logs" | grep -q "tobee is running" && echo "$logs" | grep -q "discord: connected"; then
              echo "tobee booted"; break
            fi
            st="$(docker inspect -f '{{.State.Status}}' "$tcid")"
            case "$st" in
              exited|dead) echo "tobee exited during boot" >&2; docker logs --tail=50 "$tcid" >&2; exit 1 ;;
            esac
            [ "$(date +%s)" -ge "$deadline" ] && { echo "timed out waiting for tobee boot log" >&2; echo "$logs" | tail -50 >&2; exit 1; }
            sleep 2
          done
          echo "smoke test passed"
        '''
      }
    }
  }

  post {
    always {
      // Post-build notification to Discord. Requires the Discord Notifier
      // plugin and a webhook URL in the tobee-discord-webhook credential.
      script {
        def result = currentBuild.currentResult
        def emoji = result == 'SUCCESS' ? ':green_circle:' :
                    result == 'FAILURE' ? ':red_circle:' : ':yellow_circle:'

        def branch = env.BRANCH_NAME ?: env.GIT_BRANCH ?: 'Main/Manual'

        def duration = currentBuild.durationString
            .replace(' and no weeks', '')
            .replace(' and counting', '')

        def commits = currentBuild.changeSets.collectMany { set ->
          set.items.collect { "> ${it.msg} (by *${it.author.fullName}*)" }
        }
        def commitText = commits ? commits.join('\n') : 'No recent changes detected.'

        def discordDescription = """**Status:** ${emoji} ${result}
**Branch:** `${branch}`
**Duration:** :stopwatch: ${duration}

**Commits:**
${commitText}"""

        discordSend(
          webhookURL: env.DISCORD_WEBHOOK,
          title: "📦 Build Alert: ${env.JOB_NAME} [Build #${env.BUILD_NUMBER}]",
          link: "${env.BUILD_URL}",
          result: "${currentBuild.currentResult}",
          description: discordDescription
        )
      }
    }
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
