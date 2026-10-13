package main

import (
	"context"
	"errors"
	"media-roller/src/media"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func getEnvInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	if v, err := strconv.Atoi(raw); err == nil && v > 0 {
		return v
	}
	log.Warn().Msgf("Invalid %s=%q, using default %d", key, raw, def)
	return def
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

type rateEntry struct {
	window time.Time
	count  int
}

var rateMu sync.Mutex
var rateHits = map[string]*rateEntry{}

// rateLimit aborts with 429 when an IP exceeds perMinute requests in a
// rolling 1-minute fixed window. Health/static are exempt (checked by path).
func rateLimit(perMinute int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/health") ||
				strings.HasPrefix(r.URL.Path, "/api/health") ||
				strings.HasPrefix(r.URL.Path, "/api/progress") ||
				strings.HasPrefix(r.URL.Path, "/static") {
				next.ServeHTTP(w, r)
				return
			}
			ip := clientIP(r)
			now := time.Now()
			rateMu.Lock()
			e, ok := rateHits[ip]
			if !ok || now.Sub(e.window) >= time.Minute {
				e = &rateEntry{window: now, count: 0}
				rateHits[ip] = e
			}
			e.count++
			count := e.count
			// Opportunistic cleanup to bound memory.
			if len(rateHits) > 5000 && now.Second()%10 == 0 {
				for k, v := range rateHits {
					if now.Sub(v.window) > 2*time.Minute {
						delete(rateHits, k)
					}
				}
			}
			rateMu.Unlock()
			if count > perMinute {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "Rate limit exceeded. Try again in a minute.", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func main() {
	// Very first line: proves the binary started even if something below fails.
	log.Info().Msg("media-roller starting...")
	log.Info().Msgf("env PORT=%q MR_PORT=%q MR_DOWNLOAD_DIR=%q",
		os.Getenv("PORT"), os.Getenv("MR_PORT"), os.Getenv("MR_DOWNLOAD_DIR"))

	// Ensure the download directory exists before anything tries to use it.
	// Matches media.getDownloadDir() logic: MR_DOWNLOAD_DIR or "downloads/".
	downloadDir := strings.TrimSpace(os.Getenv("MR_DOWNLOAD_DIR"))
	if downloadDir == "" {
		downloadDir = "downloads/"
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		log.Fatal().Err(err).Msgf("Failed to create download directory %q", downloadDir)
	}

	// Startup self-check: log everything Dokploy debugging needs.
	if cwd, err := os.Getwd(); err == nil {
		log.Info().Msgf("Working directory: %s", cwd)
	}
	for _, f := range []string{"templates/media/index.html", "templates/media/about.html", "static/css/style.css"} {
		if st, err := os.Stat(f); err != nil {
			log.Error().Msgf("SELF-CHECK missing %s: %v", f, err)
		} else {
			log.Info().Msgf("SELF-CHECK ok %s (%d bytes)", f, st.Size())
		}
	}
	if err := media.InitTemplates(); err != nil {
		log.Error().Err(err).Msg("SELF-CHECK templates failed to parse; pages will 503 until fixed")
	} else {
		log.Info().Msg("SELF-CHECK templates parsed")
	}

	// setup routes
	router := chi.NewRouter()
	router.Use(securityHeaders)
	router.Use(rateLimit(getEnvInt("MR_RATE_LIMIT_PER_MIN", 120)))
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
	// Timeouts: WriteTimeout must cover slow video streams, but bounded.
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           router,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 13, // 8KB
	}
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
		// Non-fatal: the app must stay up (health + logs) even with broken assets.
		log.Error().Msgf("Static documents directory not found: %s — serving without /static", root)
		return
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
