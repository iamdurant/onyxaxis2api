package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"onyxaxis2api/config"
	"onyxaxis2api/handlers"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	pool := handlers.NewPool(cfg)
	models := handlers.NewModelCache(pool, cfg.ModelCacheTTL)
	h := handlers.New(cfg, pool, models)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /v1/models", h.Auth(h.ListModels))
	mux.HandleFunc("GET /v1/models/{id}", h.Auth(h.GetModel))
	mux.HandleFunc("POST /v1/images/generations", h.Auth(h.ImageGenerations))
	mux.HandleFunc("POST /v1/images/edits", h.Auth(h.ImageEdits))
	mux.HandleFunc("GET /v1/images/file/{id}", h.Auth(h.ImageFile))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		// Image generations block for a minute or more upstream; give slow
		// clients room to receive the (potentially megabytes of) base64.
		ReadTimeout:  120 * time.Second,
		WriteTimeout: cfg.UpstreamTimeout + 60*time.Second,
	}

	go func() {
		log.Printf("onyxaxis2api listening on :%s", cfg.Port)
		log.Printf("  upstream : %s", cfg.BaseURL)
		log.Printf("  accounts : %d", len(cfg.Cookies))
		log.Printf("  default  : %s", cfg.DefaultModel)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("forced shutdown: %v", err)
	}
}
