package media

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
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

// downloadSlots caps concurrent yt-dlp/ffmpeg work. Others wait queued.
// Sized from MR_MAX_CONCURRENT_DOWNLOADS (default 2, clamped 1-8).
var downloadSlots = makeDownloadSlots()

func makeDownloadSlots() chan struct{} {
	n := 2
	if raw := strings.TrimSpace(os.Getenv("MR_MAX_CONCURRENT_DOWNLOADS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 1 && v <= 8 {
			n = v
		}
	}
	return make(chan struct{}, n)
}

func maxQueuedJobs() int {
	if raw := strings.TrimSpace(os.Getenv("MR_MAX_QUEUED_JOBS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 && v <= 200 {
			return v
		}
	}
	return 20
}

func busyJobs() int {
	return len(activeJobIDs())
}

// activeJobIDs returns IDs of jobs currently queued or running.
func activeJobIDs() map[string]bool {
	ids := map[string]bool{}
	downloadJobRegistry.mu.RLock()
	for id, j := range downloadJobRegistry.jobs {
		j.mu.RLock()
		s := j.Status
		j.mu.RUnlock()
		if s == "queued" || s == "running" {
			ids[id] = true
		}
	}
	downloadJobRegistry.mu.RUnlock()
	return ids
}

func StartDownloadJob(url string, args map[string]string) *DownloadJob {
	id := GetMD5Hash(url, args)

	// A (re)download is starting: make sure a pending purge can't remove the
	// folder it is about to write into.
	cancelMediaDeletion(id)

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
	// Reject with a clear "busy" job instead of letting the VPS OOM.
	// NOTE: registry write lock is already held here, so count inline
	// instead of calling busyJobs() (would deadlock on RLock).
	busy := 0
	for _, j := range downloadJobRegistry.jobs {
		j.mu.RLock()
		s := j.Status
		j.mu.RUnlock()
		if s == "queued" || s == "running" {
			busy++
		}
	}
	if busy >= cap(downloadSlots)+maxQueuedJobs() {
		job.setStatus("error", 0, "Server busy, try again in a minute", "Server busy, try again in a minute")
		downloadJobRegistry.jobs[id] = job
		downloadJobRegistry.mu.Unlock()
		log.Warn().Msgf("Rejecting download job %s: server busy", id)
		return job
	}
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

// removeDownloadJob drops a job once its media has been purged so a later
// submit of the same URL re-downloads instead of serving a missing file.
func removeDownloadJob(id string) {
	downloadJobRegistry.mu.Lock()
	delete(downloadJobRegistry.jobs, id)
	downloadJobRegistry.mu.Unlock()
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
	job.setStatus("queued", 0, "Queued, waiting for a download slot", "")
	downloadSlots <- struct{}{}
	defer func() { <-downloadSlots }()

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

// cleanupExpiredJobs bounds in-memory registry growth. Terminal jobs
// (completed/error) older than maxAge are dropped; progress can be re-polled
// by re-submitting the same URL (same deterministic job ID).
func cleanupExpiredJobs(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	downloadJobRegistry.mu.Lock()
	for id, j := range downloadJobRegistry.jobs {
		j.mu.RLock()
		s := j.Status
		updated := j.updatedAt
		j.mu.RUnlock()
		if (s == "completed" || s == "error") && updated.Before(cutoff) {
			delete(downloadJobRegistry.jobs, id)
		}
	}
	downloadJobRegistry.mu.Unlock()
}