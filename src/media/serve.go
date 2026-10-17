package media

import (
	"github.com/rs/zerolog/log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

/**
This will serve the fetched files to the client
*/

func ServeMedia(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	log.Info().Msgf("Serving file %s", id)
	if id == "" {
		http.Error(w, "Missing file ID", http.StatusBadRequest)
		return
	} else if !isValidId(id) {
		// Try to parse it just to avoid any type of directory traversal attacks
		http.Error(w, "Invalid file ID", http.StatusBadRequest)
		return
	}

	streamFileToClientById(w, r, id)
}

func streamFileToClientById(w http.ResponseWriter, r *http.Request, id string) {
	filename, err := getFileFromId(id)
	if err != nil {
		log.Error().Msgf("error getting file from id %s: %v", id, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	streamFileToClient(w, r, id, filename, downloadDisplayName(id, filepath.Base(filename)))
}

// countingResponseWriter tracks how much of a file was actually delivered so
// media is only purged after it has been fully served, never mid-preview/seek.
type countingResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (c *countingResponseWriter) WriteHeader(code int) {
	if !c.wroteHeader {
		c.status = code
		c.wroteHeader = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	if !c.wroteHeader {
		c.status = http.StatusOK
		c.wroteHeader = true
	}
	n, err := c.ResponseWriter.Write(p)
	c.bytes += int64(n)
	return n, err
}

func streamFileToClient(w http.ResponseWriter, r *http.Request, id string, filename string, displayName string) {
	// Check if file exists
	info, err := os.Stat(filename)
	if err != nil {
		log.Error().Msgf("error opening file %s: %v", filename, err)
		http.Error(w, "File not found.", 404)
		return
	}

	if strings.TrimSpace(displayName) == "" {
		displayName = filepath.Base(filename)
	}

	// Tell Cloudflare and browsers to cache this media for 7 days
	w.Header().Set("Cache-Control", "public, max-age=604800")

	// Inline for native browser playback, but named after the original
	// video title so files saved to the device are recognizable.
	// filename* carries the UTF-8 form for non-ASCII titles.
	w.Header().Set("Content-Disposition", "inline; filename=\""+displayName+"\"; filename*=UTF-8''"+url.QueryEscape(displayName))

	log.Info().Msgf("Opening file for streaming %s", filename)

	cw := &countingResponseWriter{ResponseWriter: w}

	// Mark this id as in use so no cleanup pass can delete it mid-stream.
	markServing(id)
	defer unmarkServing(id)

	// Serve the file; this natively handles Content-Type sniffing and HTTP Range requests (Partial Content 206)
	http.ServeFile(cw, r, filename)

	// Purge only after a real, complete download. In-page previews (the player)
	// pass ?preview=1 and must never evict media the user may still watch or
	// download; seeks/partial ranges likewise never trigger cleanup.
	if id == "" || r.URL.Query().Get("preview") != "" {
		return
	}
	if info.Size() > 0 && cw.bytes >= info.Size() &&
		(cw.status == http.StatusOK || cw.status == http.StatusPartialContent) {
		scheduleMediaDeletion(id)
	}
}
