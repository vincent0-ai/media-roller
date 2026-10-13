package media

import (
	"bufio"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dustin/go-humanize"
	"html/template"
	"media-roller/src/utils"
	"media-roller/templates"
	"net"
	"net/http"
	"net/url"
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
var ErrURLNotAllowed = errors.New("URL not allowed")

type Media struct {
	Id           string
	Name         string
	Title        string
	DownloadName string
	SizeInBytes  int64
	HumanSize    string
	IsAudio      bool
}

type mediaInfoJSON struct {
	Title string `json:"title"`
}

// titleFromInfoJSON reads the video title yt-dlp saved alongside the media.
func titleFromInfoJSON(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var info mediaInfoJSON
	if err := json.Unmarshal(data, &info); err != nil {
		return ""
	}
	return strings.TrimSpace(info.Title)
}

// downloadDisplayName returns a human-friendly filename (original video
// title + real extension) so files saved to the device are recognizable.
func downloadDisplayName(id, base string) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	infoPath := getMediaDirectory(id) + stem + ".info.json"
	return safeFileName(titleFromInfoJSON(infoPath), base)
}

// safeFileName keeps the title but strips characters illegal on
// Windows/macOS/Android, trims runaway length, and keeps the extension.
func safeFileName(title, fallback string) string {
	ext := filepath.Ext(fallback)
	t := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title), ext))
	if t == "" {
		return fallback
	}
	var b strings.Builder
	for _, r := range t {
		if r < 32 || strings.ContainsRune(`\/:*?"<>|`, r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	s := strings.Trim(strings.TrimSpace(b.String()), ".")
	if s == "" {
		return fallback
	}
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100])
	}
	return s + ext
}

var fetchIndexTmpl *template.Template

// InitTemplates parses HTML templates after startup logging, so a missing
// file can never kill the process before it logs anything. Called from main.
// Templates are embedded in the binary first, with a filesystem fallback
// for local development.
func InitTemplates() error {
	t, err := template.ParseFS(templates.Files, "media/index.html")
	if err != nil {
		t, err = template.ParseFiles("templates/media/index.html")
	}
	if err != nil {
		return err
	}
	fetchIndexTmpl = t
	return initAboutTemplates()
}

func renderIndex(w http.ResponseWriter, data map[string]any) {
	if fetchIndexTmpl == nil {
		log.Error().Msg("index template not loaded")
		http.Error(w, "Server starting, retry in a few seconds", http.StatusServiceUnavailable)
		return
	}
	if err := fetchIndexTmpl.Execute(w, data); err != nil {
		log.Error().Msgf("Error rendering template: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
	}
}

// Where the media files are saved. Always has a trailing slash
var downloadDir = getDownloadDir()
var idCharSet = regexp.MustCompile(`^[a-zA-Z0-9]+$`).MatchString
var ytDlpProgressRegexp = regexp.MustCompile(`^\[download\]\s+(\d+(?:\.\d+)?)%`)
var ytDlpStageRegexp = regexp.MustCompile(`^\[(ExtractAudio|Merger|FixupM4a|VideoConvertor)\]`)

func Index(w http.ResponseWriter, _ *http.Request) {
	renderIndex(w, map[string]any{
		"ytDlpVersion": CachedYtDlpVersion,
	})
}

func FetchMedia(w http.ResponseWriter, r *http.Request) {
	url, args := getUrl(r)
	if url == "" {
		renderIndex(w, map[string]any{
			"url":          url,
			"ytDlpVersion": CachedYtDlpVersion,
		})
		return
	}

	isAudio := false
	if _, has := args["-x"]; has {
		isAudio = true
	} else if _, has := args["--extract-audio"]; has {
		isAudio = true
	}

	job := StartDownloadJob(url, args)
	quality := args["quality"]
	if quality == "" {
		quality = "720"
	}
	renderIndex(w, map[string]any{
		"url":          url,
		"ytDlpVersion": CachedYtDlpVersion,
		"isAudio":      isAudio,
		"quality":      quality,
		"preset":       args["preset"],
		"job":          job.Snapshot(),
	})
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

	// Quality selector: 360/480/720/1080/best. Defaults to 720p to save
	// bandwidth + avoid forced re-encodes on the VPS.
	switch q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("quality"))); q {
	case "360", "480", "720", "1080", "best":
		args["quality"] = q
	case "":
		args["quality"] = "720"
	default:
		args["quality"] = "720"
	}

	// iOS preset: H.264/AAC output savable directly to Photos.
	if preset := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("preset"))); preset != "" {
		args["preset"] = preset
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
	if err := validateDownloadURL(url); err != nil {
		return nil, err.Error(), err
	}
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

		if strings.Contains(line, "could not find") && strings.Contains(line, "cookies database") {
			return "Browser cookies not found. Make sure you're logged into your browser (Brave, Chrome, Firefox, or Edge) and try again."
		}

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
	// 1. Try downloading without cookies first
	id, errMsg, err := downloadMediaWithBrowser(url, requestArgs, "", updateProgress)
	if err == nil {
		return id, "", nil
	}

	// If the failure is not related to cookies/auth, return the error immediately
	if !isCookiesOrAuthError(errMsg) {
		return "", errMsg, err
	}

	// 2. Try with browsers to retrieve cookies
	browsers := []string{"chrome", "brave", "safari"}

	if customBrowser := strings.TrimSpace(os.Getenv("MR_COOKIES_FROM_BROWSER")); customBrowser != "" {
		browsers = []string{customBrowser}
	}

	var lastErr error
	var lastErrMsg string

	for _, browser := range browsers {
		id, errMsg, err = downloadMediaWithBrowser(url, requestArgs, browser, updateProgress)
		if err == nil {
			return id, "", nil
		}

		lastErr = err
		lastErrMsg = errMsg

		if len(browsers) > 1 && updateProgress != nil {
			updateProgress(0, fmt.Sprintf("Retrying with %s", browser))
		}
		log.Info().Msgf("Cookies failed for browser %s, trying next: %v", browser, err)
	}

	return "", lastErrMsg, lastErr
}

func isCookiesOrAuthError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "cookies") ||
		strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "sign in") ||
		strings.Contains(lower, "logged in") ||
		strings.Contains(lower, "private video") ||
		strings.Contains(lower, "age-restricted")
}

func findBrowserIndex(browser string, browsers []string) int {
	for i, b := range browsers {
		if b == browser {
			return i
		}
	}
	return -1
}

func downloadMediaWithBrowser(url string, requestArgs map[string]string, browser string, updateProgress func(int, string)) (string, string, error) {
	cookieFile, cookieErr := configuredCookieFile(requestArgs)
	if cookieErr != nil {
		message := "Unable to access cookies.txt in MR_COOKIES_DIR. Check the file and directory permissions."
		return "", message, errors.New(message)
	}

	id := GetMD5Hash(url, requestArgs)
	name := getMediaDirectory(id) + "%(id)s.%(ext)s"

	if browser != "" {
		log.Info().Msgf("Downloading %s to %s with browser=%s", url, name, browser)
		if updateProgress != nil {
			updateProgress(0, fmt.Sprintf("Starting download with %s", browser))
		}
	} else {
		log.Info().Msgf("Downloading %s to %s", url, name)
		if updateProgress != nil {
			updateProgress(0, "Starting download")
		}
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
	} else if strings.ToLower(strings.TrimSpace(requestArgs["preset"])) == "ios" {
		// iOS preset: prefer natively Photos-compatible streams; the
		// post-download ffmpeg pass guarantees H.264/AAC + faststart.
		defaultArgs["--format"] = "bv*[vcodec^=avc1][ext=mp4]+ba[acodec^=mp4a]/b[ext=mp4][vcodec^=avc1]/best[ext=mp4]/best"
	} else {
		// Cap height to the requested quality. Remux (not recode) keeps
		// CPU low; h264 sort keeps max device compatibility.
		switch requestArgs["quality"] {
		case "best":
			defaultArgs["--format"] = "bestvideo[ext=mp4]+bestaudio[ext=m4a]/best[ext=mp4]/best"
		case "1080":
			defaultArgs["--format"] = "bestvideo[height<=1080][ext=mp4]+bestaudio[ext=m4a]/best[height<=1080][ext=mp4]/best[height<=1080]/best"
		case "480":
			defaultArgs["--format"] = "bestvideo[height<=480][ext=mp4]+bestaudio[ext=m4a]/best[height<=480][ext=mp4]/best[height<=480]/best"
		case "360":
			defaultArgs["--format"] = "bestvideo[height<=360][ext=mp4]+bestaudio[ext=m4a]/best[height<=360][ext=mp4]/best[height<=360]/best"
		default: // "720"
			defaultArgs["--format"] = "bestvideo[height<=720][ext=mp4]+bestaudio[ext=m4a]/best[height<=720][ext=mp4]/best[height<=720]/best"
		}
		defaultArgs["--merge-output-format"] = "mp4"
		defaultArgs["--remux-video"] = "mp4"
		defaultArgs["--format-sort"] = "codec:h264"
	}

	args := make([]string, 0)

	for arg, value := range defaultArgs {
		if _, has := requestArgs[arg]; !has {
			args = append(args, arg)
			if value != "" {
				args = append(args, value)
			}
		}
	}

	for arg, value := range requestArgs {
		if arg == "quality" || arg == "preset" {
			continue // internal only, not yt-dlp flags
		}
		args = append(args, arg)
		if value != "" {
			args = append(args, value)
		}
	}

	envVars := getEnvVars()
	if browser != "" {
		envVars["--cookies-from-browser"] = browser
	}
	for arg, value := range envVars {
		if _, has := requestArgs[arg]; !has {
			args = append(args, arg)
			if value != "" {
				args = append(args, value)
			}
		}
	}

	args = append(args, url)
	if cookieFile != "" {
		args = append([]string{"--cookies", cookieFile}, args...)
	}

	cmd := exec.Command("yt-dlp", args...)

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutIn, _ := cmd.StdoutPipe()
	stderrIn, _ := cmd.StderrPipe()

	var errStdout, errStderr error

	// Downloader diagnostics can contain cookie values: never echo them
	// to the console when a cookie file is in play.
	var consoleOut, consoleErr io.Writer
	if cookieFile == "" {
		consoleOut, consoleErr = os.Stdout, os.Stderr
	}

	err := cmd.Start()
	if err != nil {
		log.Error().Msgf("Error starting command: %v", err)
		return "", err.Error(), err
	}

	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		errStdout = scanCommandOutput(stdoutIn, &stdoutBuf, consoleOut, updateProgress)
	}()
	go func() {
		defer wg.Done()
		errStderr = scanCommandOutput(stderrIn, &stderrBuf, consoleErr, updateProgress)
	}()
	wg.Wait()
	log.Info().Msgf("Done with %s", id)

	err = cmd.Wait()
	if err != nil {
		log.Error().Err(err).Msgf("cmd.Run() failed with %s", err)
		if cookieFile != "" {
			message := "Download failed with cookies. Check that cookies.txt is writable and contains a valid, fresh cookie export, or remove it to retry without cookies."
			return "", message, errors.New(message)
		}
		fullError := strings.TrimSpace(stderrBuf.String())
		userMessage := extractUserFriendlyError(fullError)
		log.Error().Msgf("Full error output for browser=%s: %s", browser, fullError)
		return "", userMessage, err
	} else if errStdout != nil {
		log.Error().Msgf("failed to capture stdout: %v", errStdout)
	} else if errStderr != nil {
		log.Error().Msgf("failed to capture stderr: %v", errStderr)
	}

	if strings.ToLower(strings.TrimSpace(requestArgs["preset"])) == "ios" {
		if updateProgress != nil {
			updateProgress(100, "Converting for iOS")
		}
		if err := ensureIosCompatible(id); err != nil {
			message := "Downloaded media could not be converted to iOS-compatible H.264/AAC MP4."
			log.Error().Err(err).Msg(message)
			return "", message, err
		}
	}

	return id, "", nil
}

func ensureIosCompatible(id string) error {
	files, err := getAllFilesForId(id)
	if err != nil {
		return err
	}
	for _, media := range files {
		input := getMediaDirectory(id) + media.Name
		tmp := input + ".ios.tmp.mp4"
		cmd := exec.Command("ffmpeg", "-y", "-i", input, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", tmp)
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		if err := cmd.Run(); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("ffmpeg iOS compatibility conversion failed: %w: %s", err, strings.TrimSpace(stderrBuf.String()))
		}
		if err := os.Rename(tmp, input); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
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
			// Prefer the original video title from the info manifest.
			infoPath := root + strings.TrimSuffix(f, filepath.Ext(f)) + ".info.json"
			if title := titleFromInfoJSON(infoPath); title != "" {
				media.Title = title
			} else {
				media.Title = media.Name
			}
			media.DownloadName = downloadDisplayName(id, media.Name)
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

// validateDownloadURL blocks SSRF targets (localhost, private ranges,
// cloud metadata) and non-http(s) schemes before yt-dlp ever runs.
func validateDownloadURL(raw string) error {
	if len(raw) == 0 || len(raw) > 2048 {
		return ErrURLNotAllowed
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ErrURLNotAllowed
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrURLNotAllowed
	}
	if u.User != nil {
		return ErrURLNotAllowed
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return ErrURLNotAllowed
	}
	if host == "localhost" || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") {
		return ErrURLNotAllowed
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return ErrURLNotAllowed
		}
		// 100.64.0.0/10 (CGNAT) and cloud metadata endpoint.
		if ip.IsUnspecified() || host == "169.254.169.254" {
			return ErrURLNotAllowed
		}
	}
	return nil
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
	// Refuse single files bigger than this so one 4K video can't eat the VPS.
	if ev := strings.TrimSpace(os.Getenv("MR_MAX_FILESIZE")); ev != "" {
		vars["--max-filesize"] = ev
	} else {
		vars["--max-filesize"] = "500M"
	}
	return vars
}

// cacheTTL is how long finished downloads stay on disk.
// MR_CACHE_TTL_HOURS, default 6.
func cacheTTL() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("MR_CACHE_TTL_HOURS")); raw != "" {
		if h, err := strconv.Atoi(raw); err == nil && h > 0 && h <= 720 {
			return time.Duration(h) * time.Hour
		}
		log.Warn().Msgf("Invalid MR_CACHE_TTL_HOURS=%q, using default", raw)
	}
	return 6 * time.Hour
}

// maxCacheBytes caps total cache size. MR_MAX_CACHE_MB, default 2048.
// A value of 0 disables the quota (age cleanup still applies).
func maxCacheBytes() int64 {
	if raw := strings.TrimSpace(os.Getenv("MR_MAX_CACHE_MB")); raw != "" {
		if mb, err := strconv.Atoi(raw); err == nil && mb >= 0 {
			return int64(mb) * 1024 * 1024
		}
		log.Warn().Msgf("Invalid MR_MAX_CACHE_MB=%q, using default", raw)
	}
	return 2048 * 1024 * 1024
}

func dirSizeBytes(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

type cacheEntry struct {
	path    string
	modTime time.Time
	size    int64
}

// CleanOldCache deletes cache directories older than the TTL, then evicts
// oldest-first while the cache is over quota. Logs usage every run so
// Dokploy logs double as a storage monitor.
func CleanOldCache() {
	log.Info().Msg("Starting cache cleanup...")
	entries, err := os.ReadDir(downloadDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error().Err(err).Msg("Failed to read download directory for cleanup")
		}
		return
	}

	threshold := time.Now().Add(-cacheTTL())
	removed := 0
	// Never touch folders belonging to in-flight downloads.
	active := activeJobIDs()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if active[entry.Name()] {
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
				removed++
			}
		}
	}

	// Quota pass: keep total under MR_MAX_CACHE_MB, oldest first.
	quota := maxCacheBytes()
	var kept []cacheEntry
	var total int64
	if quota > 0 {
		entries, err = os.ReadDir(downloadDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					continue
				}
				p := filepath.Join(downloadDir, entry.Name())
				sz := dirSizeBytes(p)
				kept = append(kept, cacheEntry{path: p, modTime: info.ModTime(), size: sz})
				total += sz
			}
			sort.Slice(kept, func(i, j int) bool { return kept[i].modTime.Before(kept[j].modTime) })
			for _, e := range kept {
				if total <= quota {
					break
				}
				if active[filepath.Base(e.path)] {
					continue
				}
				if err := os.RemoveAll(e.path); err != nil {
					log.Error().Err(err).Msgf("Failed to evict cache directory: %s", e.path)
				} else {
					total -= e.size
					removed++
				}
			}
		}
	}

	quotaDesc := "disabled"
	if quota > 0 {
		quotaDesc = strconv.Itoa(int(quota/1024/1024)) + " MB"
	}
	log.Info().Msgf("Cache cleanup complete. Removed %d directories. Cache now %.1f MB (TTL %s, quota %s).",
		removed, float64(total)/1024/1024, cacheTTL().String(), quotaDesc)
}

// StartCacheCleanup runs the cleanup routine every hour
func StartCacheCleanup() {
	// Run once on startup
	CleanOldCache()
	cleanupExpiredJobs(2 * time.Hour)

	ticker := time.NewTicker(1 * time.Hour)
	go func() {
		for range ticker.C {
			CleanOldCache()
			cleanupExpiredJobs(2 * time.Hour)
		}
	}()
}
