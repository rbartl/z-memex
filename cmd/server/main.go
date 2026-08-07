package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"

	"gitlab.com/zollneritsch/memex/internal/chat"
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

	// Run database migrations before anything else.
	pgURL := mustEnv("POSTGRES_URL")
	if err := runMigrations(pgURL); err != nil {
		slog.Error("failed to run migrations", "err", err)
		os.Exit(1)
	}

	pg, err := storage.NewPostgres(ctx, pgURL)
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
			extractModel = "gemma4:26b"
		}
		extractor = extraction.NewOllama(ollamaURL, extractModel)
		slog.Info("using ollama extractor", "model", extractModel)
	}

	svc := memory.NewService(pg, embedder, extractor)

	chatModel := os.Getenv("OLLAMA_CHAT_MODEL")
	if chatModel == "" {
		chatModel = "llama3.2"
	}
	chatClient := chat.NewClient(ollamaURL, chatModel)
	slog.Info("using ollama chat model", "model", chatModel)

	authToken := mustEnv("MCP_AUTH_TOKEN")
	mcpServer := mcp.NewServer(svc, authToken)
	webHandler := web.NewHandler(svc, chatClient, "web", mustEnv("UI_USER"), mustEnv("UI_PASS"))

	r := chi.NewRouter()
	r.Mount("/", mcpServer.Router())
	r.Mount("/ui", webHandler.Router())

	addr := ":3000"
	httpServer := &http.Server{
		Addr:    addr,
		Handler: r,
		// Bound how long a client may take to send its headers, and how long an
		// idle keep-alive connection is held, so a slow-loris client can't tie
		// up connections. WriteTimeout is deliberately unset: the chat UI
		// streams SSE responses that outlive any fixed write deadline.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
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

func runMigrations(pgURL string) error {
	db, err := sql.Open("pgx", pgURL)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return err
	}
	slog.Info("database migrations applied")
	return nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required env var", "key", key)
		os.Exit(1)
	}
	return v
}
