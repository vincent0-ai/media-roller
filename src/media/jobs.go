package media

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

type DownloadJobSnapshot struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Progress int     `json:"progress"`
	Message  string  `json:"message"`
	Error    string  `json:"error,omitempty"`
	Media    []Media `json:"media,omitempty"`
}

type DownloadJob struct {
	mu        sync.RWMutex
	ID        string
	URL       string
	Args      map[string]string
	Status    string
	Progress  int
	Message   string
	Error     string
	Media     []Media
	createdAt time.Time
	updatedAt time.Time
}

var downloadJobRegistry = struct {
	mu   sync.RWMutex
	jobs map[string]*DownloadJob
}{jobs: map[string]*DownloadJob{}}

func StartDownloadJob(url string, args map[string]string) *DownloadJob {
	id := GetMD5Hash(url, args)

	downloadJobRegistry.mu.Lock()
	job, exists := downloadJobRegistry.jobs[id]
	if exists {
		job.mu.RLock()
		status := job.Status
		job.mu.RUnlock()
		if status == "queued" || status == "running" || status == "completed" {
			downloadJobRegistry.mu.Unlock()
			return job
		}
	}

	job = newDownloadJob(id, url, args)
	downloadJobRegistry.jobs[id] = job
	downloadJobRegistry.mu.Unlock()

	go runDownloadJob(job)
	return job
}

func getDownloadJob(id string) (*DownloadJob, bool) {
	downloadJobRegistry.mu.RLock()
	job, ok := downloadJobRegistry.jobs[id]
	downloadJobRegistry.mu.RUnlock()
	return job, ok
}

func newDownloadJob(id string, url string, args map[string]string) *DownloadJob {
	argsCopy := make(map[string]string, len(args))
	for key, value := range args {
		argsCopy[key] = value
	}

	now := time.Now()
	return &DownloadJob{
		ID:        id,
		URL:       url,
		Args:      argsCopy,
		Status:    "queued",
		Progress:  0,
		Message:   "Queued",
		createdAt: now,
		updatedAt: now,
	}
}

func runDownloadJob(job *DownloadJob) {
	job.setStatus("running", 0, "Starting download", "")
	medias, ytdlpErrorMessage, err := getMediaResultsWithProgress(job.URL, job.Args, job.updateProgress)
	if err != nil {
		message := ytdlpErrorMessage
		if message == "" {
			message = err.Error()
		}
		job.setStatus("error", job.ProgressValue(), message, message)
		log.Error().Err(err).Msgf("Download job %s failed", job.ID)
		return
	}

	job.mu.Lock()
	job.Status = "completed"
	job.Progress = 100
	job.Message = "Done"
	job.Error = ""
	job.Media = append([]Media(nil), medias...)
	job.updatedAt = time.Now()
	job.mu.Unlock()
}

func (job *DownloadJob) updateProgress(progress int, message string) {
	job.mu.Lock()
	if progress > job.Progress {
		job.Progress = progress
	}
	if message != "" {
		job.Message = message
	}
	job.Status = "running"
	job.updatedAt = time.Now()
	job.mu.Unlock()
}

func (job *DownloadJob) setStatus(status string, progress int, message string, errMessage string) {
	job.mu.Lock()
	job.Status = status
	job.Progress = progress
	job.Message = message
	job.Error = errMessage
	job.updatedAt = time.Now()
	job.mu.Unlock()
}

func (job *DownloadJob) ProgressValue() int {
	job.mu.RLock()
	defer job.mu.RUnlock()
	return job.Progress
}

func (job *DownloadJob) Snapshot() *DownloadJobSnapshot {
	job.mu.RLock()
	defer job.mu.RUnlock()

	snapshot := &DownloadJobSnapshot{
		ID:       job.ID,
		Status:   job.Status,
		Progress: job.Progress,
		Message:  job.Message,
		Error:    job.Error,
	}
	if len(job.Media) > 0 {
		snapshot.Media = append([]Media(nil), job.Media...)
	}

	return snapshot
}

func (job *DownloadJob) writeSnapshot(w http.ResponseWriter) error {
	return json.NewEncoder(w).Encode(job.Snapshot())
}