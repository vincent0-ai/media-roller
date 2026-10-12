package main

import (
	"context"
	"errors"
	"media-roller/src/media"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
)

func getPort() string {
	// MR_PORT takes precedence (app-specific), then PORT (platform-injected
	// by Railway/Render/Fly/Dokploy/Coolify/Easypanel/etc), then default 3000.
	for _, key := range []string{"MR_PORT", "PORT"} {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			if p, err := strconv.Atoi(raw); err == nil && p > 0 && p < 65536 {
				return strconv.Itoa(p)
			}
			log.Warn().Msgf("Invalid %s=%q, falling back to default port", key, raw)
		}
	}
	return "3000"
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	// Ensure the download directory exists before anything tries to use it.
	// Matches media.getDownloadDir() logic: MR_DOWNLOAD_DIR or "downloads/".
	downloadDir := strings.TrimSpace(os.Getenv("MR_DOWNLOAD_DIR"))
	if downloadDir == "" {
		downloadDir = "downloads/"
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		log.Fatal().Err(err).Msgf("Failed to create download directory %q", downloadDir)
	}

	// Log working directory to make missing templates/static easy to diagnose.
	if cwd, err := os.Getwd(); err == nil {
		log.Info().Msgf("Working directory: %s", cwd)
	}

	// setup routes
	router := chi.NewRouter()
	router.Get("/health", healthHandler)
	router.Get("/api/health", healthHandler)
	router.Route("/", func(r chi.Router) {
		router.Get("/", media.Index)
		router.Get("/fetch", media.FetchMedia)
		router.Get("/api/download", media.FetchMediaApi)
		router.Get("/api/progress", media.DownloadProgressApi)
		router.Get("/download", media.ServeMedia)
		router.Get("/about", media.AboutIndex)
	})
	fileServer(router, "/static", "static/")

	// Validate routes
	walkFunc := func(method string, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		return nil
	}
	if err := chi.Walk(router, walkFunc); err != nil {
		log.Panic().Msgf("Route setup error: %s", err.Error())
	}

	media.GetInstalledVersion()
	go startYtDlpUpdater()
	go media.StartCacheCleanup()

	port := getPort()
	// The HTTP Server
	server := &http.Server{Addr: ":" + port, Handler: router}
	log.Info().Msgf("Starting server on :%s (MR_DOWNLOAD_DIR=%s)", port, downloadDir)

	// Server run context
	serverCtx, serverStopCtx := context.WithCancel(context.Background())

	// Listen for syscall signals for process to interrupt/quit
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-sig

		// Shutdown signal with grace period of 30 seconds
		shutdownCtx, cancel := context.WithTimeout(serverCtx, 30*time.Second)
		defer cancel()

		go func() {
			<-shutdownCtx.Done()
			if errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) {
				log.Fatal().Msg("graceful shutdown timed out.. forcing exit.")
			}
		}()

		// Trigger graceful shutdown
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			log.Fatal().Err(err)
		}
		serverStopCtx()
	}()

	// Run the server
	err := server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal().Err(err)
	}

	// Wait for server context to be stopped
	<-serverCtx.Done()
	log.Info().Msgf("Shutdown complete")
}

// startYtDlpUpdater will update the yt-dlp to the latest nightly version ever few hours
func startYtDlpUpdater() {
	log.Info().Msgf("yt-dlp version: %s", media.GetInstalledVersion())
	ticker := time.NewTicker(12 * time.Hour)

	// Do one update now
	_ = media.UpdateYtDlp()

	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				_ = media.UpdateYtDlp()
				log.Info().Msgf("yt-dlp version: %s", media.GetInstalledVersion())
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
}

func fileServer(r chi.Router, public string, static string) {
	if strings.ContainsAny(public, "{}*") {
		panic("FileServer does not permit URL parameters.")
	}

	root, _ := filepath.Abs(static)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		log.Error().Msgf("Static documents directory not found: %s (cwd check above)", root)
		panic("Static Documents Directory Not Found: " + root)
	}

	fs := http.StripPrefix(public, http.FileServer(http.Dir(root)))

	if public != "/" && public[len(public)-1] != '/' {
		r.Get(public, http.RedirectHandler(public+"/", http.StatusMovedPermanently).ServeHTTP)
		public += "/"
	}

	r.Get(public+"*", func(w http.ResponseWriter, r *http.Request) {
		file := strings.Replace(r.RequestURI, public, "/", 1)
		if _, err := os.Stat(root + file); os.IsNotExist(err) {
			http.ServeFile(w, r, path.Join(root, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	})
}
