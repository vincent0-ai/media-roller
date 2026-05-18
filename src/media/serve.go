package media

import (
	"github.com/rs/zerolog/log"
	"net/http"
	"os"
	"path/filepath"
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
	}

	streamFileToClient(w, r, filename)
}

func streamFileToClient(w http.ResponseWriter, r *http.Request, filename string) {
	// Check if file exists
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		log.Error().Msgf("error opening file %s: %v", filename, err)
		http.Error(w, "File not found.", 404)
		return
	}

	// Tell Cloudflare and browsers to cache this media for 7 days
	w.Header().Set("Cache-Control", "public, max-age=604800")
	
	// Set Content-Disposition to inline for native browser playback
	w.Header().Set("Content-Disposition", "inline; filename=\""+filepath.Base(filename)+"\"")

	log.Info().Msgf("Opening file for streaming %s", filename)

	// Serve the file; this natively handles Content-Type sniffing and HTTP Range requests (Partial Content 206)
	http.ServeFile(w, r, filename)
}
