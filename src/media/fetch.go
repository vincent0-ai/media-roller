package media

import (
	"bufio"
	"crypto/md5"
	"errors"
	"fmt"
	"github.com/dustin/go-humanize"
	"html/template"
	"media-roller/src/utils"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

/**
This file will download the media from a URL and save it to disk.
*/

import (
	"bytes"
	"github.com/rs/zerolog/log"
	"io"
	"os"
	"os/exec"
)

var ErrMissingURL = errors.New("missing URL")

type Media struct {
	Id          string
	Name        string
	SizeInBytes int64
	HumanSize   string
	IsAudio     bool
}

var fetchIndexTmpl = template.Must(template.ParseFiles("templates/media/index.html"))

// Where the media files are saved. Always has a trailing slash
var downloadDir = getDownloadDir()
var idCharSet = regexp.MustCompile(`^[a-zA-Z0-9]+$`).MatchString
var ytDlpProgressRegexp = regexp.MustCompile(`^\[download\]\s+(\d+(?:\.\d+)?)%`)
var ytDlpStageRegexp = regexp.MustCompile(`^\[(ExtractAudio|Merger|FixupM4a|VideoConvertor)\]`)

func Index(w http.ResponseWriter, _ *http.Request) {
	data := map[string]string{
		"ytDlpVersion": CachedYtDlpVersion,
	}
	if err := fetchIndexTmpl.Execute(w, data); err != nil {
		log.Error().Msgf("Error rendering template: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
	}
}

func FetchMedia(w http.ResponseWriter, r *http.Request) {
	url, args := getUrl(r)
	if url == "" {
		data := map[string]any{
			"url":          url,
			"ytDlpVersion": CachedYtDlpVersion,
		}
		if err := fetchIndexTmpl.Execute(w, data); err != nil {
			log.Error().Msgf("Error rendering template: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
		}
		return
	}

	isAudio := false
	if _, has := args["-x"]; has {
		isAudio = true
	} else if _, has := args["--extract-audio"]; has {
		isAudio = true
	}

	job := StartDownloadJob(url, args)
	data := map[string]any{
		"url":          url,
		"ytDlpVersion": CachedYtDlpVersion,
		"isAudio":      isAudio,
		"job":          job.Snapshot(),
	}

	if err := fetchIndexTmpl.Execute(w, data); err != nil {
		log.Error().Msgf("Error rendering template: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
	}
}

func FetchMediaApi(w http.ResponseWriter, r *http.Request) {
	url, args := getUrl(r)
	medias, _, err := getMediaResults(url, args)
	if err != nil {
		if !errors.Is(err, ErrMissingURL) {
			log.Error().Msgf("error getting media results: %v", err)
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(medias) == 0 {
		log.Error().Msgf("not media found")
		http.Error(w, "Media not found", http.StatusBadRequest)
		return
	}

	// just take the first one
	streamFileToClientById(w, r, medias[0].Id)
}

func DownloadProgressApi(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "Missing job ID", http.StatusBadRequest)
		return
	}

	job, ok := getDownloadJob(id)
	if !ok {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := job.writeSnapshot(w); err != nil {
		log.Error().Msgf("Error writing job snapshot: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
	}
}

func getUrl(r *http.Request) (string, map[string]string) {
	u := strings.TrimSpace(r.URL.Query().Get("url"))

	// Support yt-dlp arguments passed in via the url. We'll assume anything starting with a dash - is an argument
	args := make(map[string]string)
	for k, v := range r.URL.Query() {
		if strings.HasPrefix(k, "-") {
			if len(v) > 0 {
				args[k] = v[0]
			} else {
				args[k] = ""
			}
		}
	}

	if strings.ToLower(r.URL.Query().Get("audio")) == "true" {
		args["-x"] = ""
	}

	return u, args
}

func getMediaResults(inputUrl string, args map[string]string) ([]Media, string, error) {
	return getMediaResultsWithProgress(inputUrl, args, nil)
}

func getMediaResultsWithProgress(inputUrl string, args map[string]string, updateProgress func(int, string)) ([]Media, string, error) {
	if inputUrl == "" {
		return nil, "", ErrMissingURL
	}

	url := utils.NormalizeUrl(inputUrl)
	log.Info().Msgf("Got input '%s' and extracted '%s' with args %v", inputUrl, url, args)

	// NOTE: This system is for a simple use case, meant to run at home. This is not a great design for a robust system.
	// We are hashing the URL here and writing files to disk to a consistent directory based on the ID. You can imagine
	// concurrent users would break this for the same URL. That's fine given this is for a simple home system.
	// Future work can make this more sophisticated.
	id := GetMD5Hash(url, args)
	// Look to see if we already have the media on disk
	medias, err := getAllFilesForId(id)
	if err != nil {
		return nil, "", err
	}
	if len(medias) > 0 {
		if updateProgress != nil {
			updateProgress(100, "Using cached files")
		}
		return medias, "", nil
	}
	if len(medias) == 0 {
		// We don't, so go fetch it
		errMessage := ""
		id, errMessage, err = downloadMediaWithProgress(url, args, updateProgress)
		if err != nil {
			return nil, errMessage, err
		}
		medias, err = getAllFilesForId(id)
		if err != nil {
			return nil, "", err
		}
	}

	return medias, "", nil
}

func extractUserFriendlyError(fullError string) string {
	lines := strings.Split(fullError, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "ERROR:") {
			if strings.Contains(line, "Sign in to confirm you're not a bot") {
				return "YouTube requires authentication. Please ensure you're logged into your browser and try again."
			}
			if strings.Contains(line, "Video unavailable") {
				return "This video is unavailable or has been removed."
			}
			if strings.Contains(line, "Private video") {
				return "This is a private video and cannot be downloaded."
			}
			if strings.Contains(line, "age-restricted") {
				return "This video is age-restricted. Please ensure you're logged in with an account that has access."
			}
			if strings.Contains(line, "403") || strings.Contains(line, "Forbidden") {
				return "Access denied. The content owner has restricted downloading."
			}
			if strings.Contains(line, "404") || strings.Contains(line, "Not Found") {
				return "Video not found. Please check the URL."
			}
			if idx := strings.Index(line, "ERROR:"); idx != -1 {
				msg := strings.TrimSpace(line[idx+6:])
				if len(msg) > 200 {
					msg = msg[:200] + "..."
				}
				return msg
			}
		}
	}
	return "Download failed. Please check the URL and try again."
}

// returns the ID of the file, and error message, and an error
func downloadMedia(url string, requestArgs map[string]string) (string, string, error) {
	return downloadMediaWithProgress(url, requestArgs, nil)
}

func downloadMediaWithProgress(url string, requestArgs map[string]string, updateProgress func(int, string)) (string, string, error) {
	// The id will be used as the name of the parent directory of the output files
	id := GetMD5Hash(url, requestArgs)
	name := getMediaDirectory(id) + "%(id)s.%(ext)s"

	log.Info().Msgf("Downloading %s to %s", url, name)
	if updateProgress != nil {
		updateProgress(0, "Starting yt-dlp")
	}

	isAudioOnly := false
	if _, has := requestArgs["-x"]; has {
		isAudioOnly = true
	} else if _, has := requestArgs["--extract-audio"]; has {
		isAudioOnly = true
	}

	defaultArgs := map[string]string{
		"--trim-filenames":     "100",
		"--restrict-filenames": "",
		"--write-info-json":    "",
		"--verbose":            "",
		"--newline":            "",
		"--output":             name,
	}

	if isAudioOnly {
		defaultArgs["--format"] = "bestaudio[ext=m4a]/bestaudio/best"
		defaultArgs["--audio-format"] = "mp3"
		defaultArgs["--audio-quality"] = "128K"
	} else {
		defaultArgs["--format"] = "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best[ext=mp4]/best"
		defaultArgs["--merge-output-format"] = "mp4"
		defaultArgs["--recode-video"] = "mp4"
		defaultArgs["--format-sort"] = "codec:h264"
	}

	args := make([]string, 0)

	// First add all default arguments that were not supplied as request level arguments
	for arg, value := range defaultArgs {
		if _, has := requestArgs[arg]; !has {
			args = append(args, arg)
			if value != "" {
				args = append(args, value)
			}
		}
	}

	// Now add all request level arguments
	for arg, value := range requestArgs {
		args = append(args, arg)
		if value != "" {
			args = append(args, value)
		}
	}

	// And finally add any environment level arguments not supplied as request level args
	for arg, value := range getEnvVars() {
		if _, has := requestArgs[arg]; !has {
			args = append(args, arg)
			if value != "" {
				args = append(args, value)
			}
		}
	}

	args = append(args, url)

	cmd := exec.Command("yt-dlp", args...)

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutIn, _ := cmd.StdoutPipe()
	stderrIn, _ := cmd.StderrPipe()

	var errStdout, errStderr error

	err := cmd.Start()
	if err != nil {
		log.Error().Msgf("Error starting command: %v", err)
		return "", err.Error(), err
	}

	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		errStdout = scanCommandOutput(stdoutIn, &stdoutBuf, os.Stdout, updateProgress)
	}()
	go func() {
		defer wg.Done()
		errStderr = scanCommandOutput(stderrIn, &stderrBuf, os.Stderr, updateProgress)
	}()
	wg.Wait()
	log.Info().Msgf("Done with %s", id)

	err = cmd.Wait()
	if err != nil {
		log.Error().Err(err).Msgf("cmd.Run() failed with %s", err)
		fullError := strings.TrimSpace(stderrBuf.String())
		userMessage := extractUserFriendlyError(fullError)
		log.Error().Msgf("Full error output: %s", fullError)
		return "", userMessage, err
	} else if errStdout != nil {
		log.Error().Msgf("failed to capture stdout: %v", errStdout)
	} else if errStderr != nil {
		log.Error().Msgf("failed to capture stderr: %v", errStderr)
	}

	return id, "", nil
}

func scanCommandOutput(reader io.Reader, buffer *bytes.Buffer, writer io.Writer, updateProgress func(int, string)) error {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if buffer != nil {
			buffer.WriteString(line)
			buffer.WriteByte('\n')
		}
		if writer != nil {
			_, _ = fmt.Fprintln(writer, line)
		}
		if updateProgress != nil {
			if percent, message, ok := parseYtDlpProgressLine(line); ok {
				updateProgress(percent, message)
			} else if message, ok := parseYtDlpStageLine(line); ok {
				updateProgress(100, message)
			}
		}
	}
	return scanner.Err()
}

func parseYtDlpProgressLine(line string) (int, string, bool) {
	matches := ytDlpProgressRegexp.FindStringSubmatch(line)
	if len(matches) != 2 {
		return 0, "", false
	}

	percentValue, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, "", false
	}

	return int(percentValue + 0.5), "Downloading media", true
}

func parseYtDlpStageLine(line string) (string, bool) {
	if !ytDlpStageRegexp.MatchString(line) {
		return "", false
	}

	if strings.Contains(line, "ExtractAudio") {
		return "Converting audio", true
	}
	if strings.Contains(line, "Merger") {
		return "Merging formats", true
	}
	if strings.Contains(line, "FixupM4a") {
		return "Finalizing audio", true
	}
	if strings.Contains(line, "VideoConvertor") {
		return "Converting video", true
	}

	return "Processing media", true
}

// Returns the relative directory containing the media file, with a trailing slash.
// Id is expected to be pre validated
func getMediaDirectory(id string) string {
	return downloadDir + id + "/"
}

// id is expected to be validated prior to calling this func
func getAllFilesForId(id string) ([]Media, error) {
	root := getMediaDirectory(id)
	file, err := os.Open(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	files, _ := file.Readdirnames(0) // 0 to read all files and folders
	if len(files) == 0 {
		return nil, errors.New("ID not found: " + id)
	}

	var medias []Media

	// We expect two files to be produced for each video, a json manifest and an mp4.
	for _, f := range files {
		if !strings.HasSuffix(f, ".json") {
			fi, err2 := os.Stat(root + f)
			var size int64 = 0
			if err2 == nil {
				size = fi.Size()
			}

			lowerF := strings.ToLower(f)
			media := Media{
				Id:          id,
				Name:        filepath.Base(f),
				SizeInBytes: size,
				HumanSize:   humanize.Bytes(uint64(size)),
				IsAudio:     strings.HasSuffix(lowerF, ".mp3") || strings.HasSuffix(lowerF, ".m4a") || strings.HasSuffix(lowerF, ".wav") || strings.HasSuffix(lowerF, ".flac"),
			}
			medias = append(medias, media)
		}
	}

	return medias, nil
}

// id is expected to be validated prior to calling this func
// TODO: This needs to handle multiple files in the directory
func getFileFromId(id string) (string, error) {
	root := getMediaDirectory(id)
	file, err := os.Open(root)
	if err != nil {
		return "", err
	}
	files, _ := file.Readdirnames(0) // 0 to read all files and folders
	if len(files) == 0 {
		return "", errors.New("ID not found")
	}

	// We expect two files to be produced, a json manifest and an mp4. We want to return the mp4
	// Sometimes the video file might not have an mp4 extension, so filter out the json file
	for _, f := range files {
		if !strings.HasSuffix(f, ".json") {
			// TODO: This is just returning the first file found. We need to handle multiple
			return root + f, nil
		}
	}

	return "", errors.New("unable to find file")
}

func GetMD5Hash(url string, args map[string]string) string {
	id := url
	if len(args) > 0 {
		tmp := make([]string, 0)
		for k, v := range args {
			tmp = append(tmp, k, v)
		}
		sort.Strings(tmp)
		id += ":" + strings.Join(tmp, ",")
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(id)))
}

func isValidId(id string) bool {
	return idCharSet(id)
}

func getDownloadDir() string {
	dir := os.Getenv("MR_DOWNLOAD_DIR")
	if dir != "" {
		if !strings.HasSuffix(dir, "/") {
			return dir + "/"
		}
		return dir
	}
	return "downloads/"
}

func getEnvVars() map[string]string {
	vars := make(map[string]string)
	if ev := strings.TrimSpace(os.Getenv("MR_PROXY")); ev != "" {
		vars["--proxy"] = ev
	}
	if ev := strings.TrimSpace(os.Getenv("MR_COOKIES_FROM_BROWSER")); ev != "" {
		vars["--cookies-from-browser"] = ev
	} else {
		vars["--cookies-from-browser"] = "chromium"
	}
	return vars
}

// CleanOldCache deletes downloaded media directories older than 1 day
func CleanOldCache() {
	log.Info().Msg("Starting cache cleanup...")
	entries, err := os.ReadDir(downloadDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error().Err(err).Msg("Failed to read download directory for cleanup")
		}
		return
	}

	threshold := time.Now().Add(-24 * time.Hour)
	count := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(threshold) {
			dirPath := filepath.Join(downloadDir, entry.Name())
			if err := os.RemoveAll(dirPath); err != nil {
				log.Error().Err(err).Msgf("Failed to delete old cache directory: %s", dirPath)
			} else {
				log.Info().Msgf("Deleted old cache directory: %s", dirPath)
				count++
			}
		}
	}
	log.Info().Msgf("Cache cleanup complete. Removed %d old directories.", count)
}

// StartCacheCleanup runs the cleanup routine once a day
func StartCacheCleanup() {
	// Run once on startup
	CleanOldCache()

	ticker := time.NewTicker(24 * time.Hour)
	go func() {
		for range ticker.C {
			CleanOldCache()
		}
	}()
}
