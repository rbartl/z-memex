package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"

	"gitlab.com/zollneritsch/memex/internal/embeddings"
	"gitlab.com/zollneritsch/memex/internal/extraction"
	"gitlab.com/zollneritsch/memex/internal/mcp"
	"gitlab.com/zollneritsch/memex/internal/memory"
	"gitlab.com/zollneritsch/memex/internal/storage"
	"gitlab.com/zollneritsch/memex/web"
)

func main() {
	_ = godotenv.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx := context.Background()

	pg, err := storage.NewPostgres(ctx, mustEnv("POSTGRES_URL"))
	if err != nil {
		slog.Error("failed to connect to postgres", "err", err)
		os.Exit(1)
	}
	defer pg.Close()

	ollamaURL := mustEnv("OLLAMA_URL")
	embedder := embeddings.NewOllama(ollamaURL, "nomic-embed-text")

	var extractor memory.Extractor
	switch os.Getenv("EXTRACTOR") {
	case "anthropic":
		extractor = extraction.NewAnthropic(mustEnv("ANTHROPIC_API_KEY"))
	default: // "ollama" or empty
		extractModel := os.Getenv("OLLAMA_EXTRACT_MODEL")
		if extractModel == "" {
			extractModel = "qwen3.5:9b"
		}
		extractor = extraction.NewOllama(ollamaURL, extractModel)
		slog.Info("using ollama extractor", "model", extractModel)
	}

	svc := memory.NewService(pg, embedder, extractor)

	authToken := mustEnv("MCP_AUTH_TOKEN")
	mcpServer := mcp.NewServer(svc, authToken)
	webHandler := web.NewHandler(svc, "web", mustEnv("UI_USER"), mustEnv("UI_PASS"))

	r := chi.NewRouter()
	r.Mount("/", mcpServer.Router())
	r.Mount("/ui", webHandler.Router())

	addr := ":3000"
	httpServer := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	go func() {
		slog.Info("memex server starting", "addr", addr, "ui", "http://localhost:3000/ui")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down...")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutCtx)
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required env var", "key", key)
		os.Exit(1)
	}
	return v
}
