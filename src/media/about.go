package media

import (
	"html/template"
	"media-roller/src/utils"
	"net/http"
	"regexp"
	"strings"

	"github.com/matishsiao/goInfo"
	"github.com/rs/zerolog/log"
)

var aboutIndexTmpl *template.Template

func initAboutTemplates() error {
	t, err := template.ParseFiles("templates/media/about.html")
	if err != nil {
		return err
	}
	aboutIndexTmpl = t
	return nil
}

var newlineRegex = regexp.MustCompile("\r?\n")

func AboutIndex(w http.ResponseWriter, _ *http.Request) {
	pythonVersion := utils.RunCommand("python3", "--version")
	if pythonVersion == "" {
		pythonVersion = utils.RunCommand("python", "--version")
	}

	gi, _ := goInfo.GetInfo()

	data := map[string]any{
		"ytDlpVersion":   CachedYtDlpVersion,
		"goVersion":      strings.TrimPrefix(utils.RunCommand("go", "version"), "go version "),
		"pythonVersion":  strings.TrimPrefix(pythonVersion, "Python "),
		"ffmpegVersion":  newlineRegex.Split(utils.RunCommand("ffmpeg", "-version"), -1),
		"ffprobeVersion": newlineRegex.Split(utils.RunCommand("ffprobe", "-version"), -1),
		"deno":           strings.TrimPrefix(utils.RunCommand("deno", "--version"), "go version "),
		"os":             gi.OS,
		"kernel":         gi.Kernel,
		"core":           gi.Core,
		"platform":       gi.Platform,
		"hostname":       gi.Hostname,
		"cpus":           gi.CPUs,
	}

	if aboutIndexTmpl == nil {
		log.Error().Msg("about template not loaded")
		http.Error(w, "Server starting, retry in a few seconds", http.StatusServiceUnavailable)
		return
	}
	if err := aboutIndexTmpl.Execute(w, data); err != nil {
		log.Error().Msgf("Error rendering template: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
	}
}
