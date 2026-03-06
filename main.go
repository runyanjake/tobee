package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"tobee/integrations"
	"tobee/integrations/discord"
	"tobee/integrations/memory"
	"tobee/internal/actions"
	"tobee/internal/ai"
	"tobee/internal/core"
	"tobee/internal/state"
	"tobee/internal/triggers"

	chromem "github.com/philippgille/chromem-go"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, using environment variables")
	}

	aiURL := mustEnv("AI_PROVIDER_URL")
	discordToken := mustEnv("DISCORD_TOKEN")

	aiModel := os.Getenv("AI_MODEL")
	if aiModel == "" {
		aiModel = "local-model"
	}

	// --- Core ---
	s := state.New()
	if err := s.LoadContextDir("context"); err != nil {
		log.Fatalf("loading context dir: %v", err)
	}

	actions.Register(s)

	aiClient := ai.NewClient(aiURL, aiModel)
	engine := triggers.NewEngine(s)

	chatTmpl, err := ai.LoadTemplate("prompts/chat.md")
	if err != nil {
		log.Fatalf("loading chat template: %v", err)
	}

	// --- Memory ---
	// If AI_EMBED_MODEL is set, vector search is enabled via the embedding endpoint.
	// Otherwise the store falls back to keyword search.
	var embeddingFunc chromem.EmbeddingFunc
	if embedModel := os.Getenv("AI_EMBED_MODEL"); embedModel != "" {
		embeddingFunc = chromem.NewEmbeddingFuncOpenAICompat(aiURL, "", embedModel, nil)
		log.Printf("memory: using embedding model %q", embedModel)
	}
	memStore, err := memory.NewStore(context.Background(), embeddingFunc)
	if err != nil {
		log.Fatalf("creating memory store: %v", err)
	}
	memory.Register(s, memStore)

	queue := core.NewQueue(64)
	loop := core.NewLoop(queue, s, aiClient, chatTmpl, memStore)

	// --- Integrations ---
	discordBot, err := discord.New(discordToken, s, queue, os.Getenv("DISCORD_CHANNEL_ID"))
	if err != nil {
		log.Fatalf("creating discord integration: %v", err)
	}

	integrations := []integration.Integration{
		discordBot,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, ig := range integrations {
		if err := ig.Start(ctx); err != nil {
			log.Fatalf("starting integration %q: %v", ig.Name(), err)
		}
	}

	loop.Start(ctx)
	engine.Start(ctx)

	log.Println("tobee is running — press Ctrl+C to exit")

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc

	log.Println("shutting down...")
	for _, ig := range integrations {
		if err := ig.Stop(); err != nil {
			log.Printf("stopping integration %q: %v", ig.Name(), err)
		}
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}
