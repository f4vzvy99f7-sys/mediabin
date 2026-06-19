package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type DownloadStatus string

const (
	StatusPending     DownloadStatus = "pending"
	StatusDownloading DownloadStatus = "downloading"
	StatusFinished    DownloadStatus = "finished"
	StatusError       DownloadStatus = "error"
	StatusCancelled   DownloadStatus = "cancelled"
)

type Progress struct {
	Percent         float64 `json:"percent"`
	Speed           string  `json:"speed"`
	Eta             string  `json:"eta"`
	TotalBytes      int64   `json:"total_bytes"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	Filename        string  `json:"filename"`
}

type VideoMetadata struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Uploader    string   `json:"uploader"`
	Description string   `json:"description"`
	Thumbnail   string   `json:"thumbnail"`
	Duration    int      `json:"duration"`
	WebpageURL  string   `json:"webpage_url"`
	URL         string   `json:"url"`
	Extractor   string   `json:"extractor"`
	Channel     string   `json:"channel"`
	ChannelID   string   `json:"channel_id"`
	UploadDate  string   `json:"upload_date"`
	Tags        []string `json:"tags"`
	Categories  []string `json:"categories"`
	Cast        []string `json:"cast"`

	// Computed fields — not from yt-dlp JSON output.
	MbIdentifier string `json:"-"` // MD5 hex of "{extractor}__{id}"
	MbPath       string `json:"-"` // "{id[0:2]}/{id[2:4]}/{id}"
}

// enrichMetadata computes the MbIdentifier and MbPath fields from the raw
// yt-dlp ID and extractor, matching the Python implementation's ID scheme.
func enrichMetadata(m *VideoMetadata) {
	raw := fmt.Sprintf("%s__%s", m.Extractor, m.ID)
	sum := md5.Sum([]byte(raw))
	hex := fmt.Sprintf("%x", sum)
	m.MbIdentifier = hex
	m.MbPath = fmt.Sprintf("%s/%s/%s", hex[0:2], hex[2:4], hex)
}

type Task struct {
	ID         string         `json:"id"`
	URL        string         `json:"url"`
	Status     DownloadStatus `json:"status"`
	Progress   *Progress      `json:"progress"`
	Metadata   *VideoMetadata `json:"metadata"`
	TempDir    string         `json:"temp_dir"`
	Error      error          `json:"error"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
}

type DownloadEvent struct {
	TaskID   string
	Status   DownloadStatus
	Progress *Progress
	Metadata *VideoMetadata
	TempDir  string
	Error    error
	Finished time.Time
}

var ErrNoSubscriber = errors.New("no active subscribers - at least one subscriber required to start downloads")
var ErrAlreadyDownloading = errors.New("download already in progress for this task id")

type TaskSnapshot struct {
	ID      string
	Title   string
	Status  DownloadStatus
	Percent float64
	Speed   string
	Eta     string
}

type Downloader interface {
	FetchInfo(ctx context.Context, url string) (VideoMetadata, error)
	StartDownload(ctx context.Context, id string, url string) error
	Subscribe() chan DownloadEvent
	Unsubscribe(ch chan DownloadEvent)
	GetActiveTasks() []TaskSnapshot
}

type ytdlpDownloader struct {
	ctx          context.Context
	mu           sync.RWMutex
	subscribers  map[chan DownloadEvent]bool
	jobCh        chan downloadStartJob
	stderrWriter io.Writer
	activeTasks  sync.Map
}

type downloadStartJob struct {
	id          string
	url         string
	isDuplicate chan bool
}

type taskState struct {
	ID        string
	URL       string
	Status    DownloadStatus
	Progress  *Progress
	Metadata  *VideoMetadata
	TempDir   string
	Error     error
	StartedAt time.Time
}

func NewDownloader(ctx context.Context, stderrWriter io.Writer) Downloader {
	d := &ytdlpDownloader{
		ctx:          ctx,
		subscribers:  make(map[chan DownloadEvent]bool),
		jobCh:        make(chan downloadStartJob),
		stderrWriter: stderrWriter,
	}

	for range 3 {
		go d.worker()
	}

	return d
}

func (d *ytdlpDownloader) worker() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case job, ok := <-d.jobCh:
			if !ok {
				return
			}

			task := &taskState{
				ID:        job.id,
				URL:       job.url,
				Status:    StatusPending,
				StartedAt: time.Now(),
			}

			_, loaded := d.activeTasks.LoadOrStore(job.id, task)
			if loaded {
				job.isDuplicate <- true
				continue
			}
			job.isDuplicate <- false

			d.executeDownload(task)
			d.activeTasks.Delete(job.id)
		}
	}
}

func (d *ytdlpDownloader) FetchInfo(ctx context.Context, url string) (VideoMetadata, error) {
	args := []string{
		"--dump-json",
		"--no-download",
		"--no-warnings",
		"--quiet",
		url,
	}

	cmd := exec.CommandContext(ctx, GetYtdlpPath(), args...)
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return VideoMetadata{}, ctx.Err()
		}
		return VideoMetadata{}, fmt.Errorf("yt-dlp failed: %w", err)
	}

	var metadata VideoMetadata
	if err := json.Unmarshal(output, &metadata); err != nil {
		return VideoMetadata{}, fmt.Errorf("failed to parse yt-dlp output: %w", err)
	}

	enrichMetadata(&metadata)
	return metadata, nil
}

func (d *ytdlpDownloader) StartDownload(ctx context.Context, id string, url string) error {
	d.mu.RLock()
	if len(d.subscribers) == 0 {
		d.mu.RUnlock()
		return ErrNoSubscriber
	}
	d.mu.RUnlock()

	isDuplicate := make(chan bool, 1)
	job := downloadStartJob{id: id, url: url, isDuplicate: isDuplicate}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case d.jobCh <- job:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case dup := <-isDuplicate:
		if dup {
			return ErrAlreadyDownloading
		}
		return nil
	}
}

func (d *ytdlpDownloader) executeDownload(task *taskState) {
	task.Status = StatusDownloading
	d.emitEvent(DownloadEvent{
		TaskID: task.ID,
		Status: StatusDownloading,
	})

	tempDir, err := d.doDownload(task)
	finishedAt := time.Now()

	if err != nil {
		if task.Status != StatusCancelled {
			task.Status = StatusError
			task.Error = err
		}
	} else {
		task.Status = StatusFinished
		task.TempDir = tempDir
	}

	d.emitEvent(DownloadEvent{
		TaskID:   task.ID,
		Status:   task.Status,
		Metadata: task.Metadata,
		TempDir:  task.TempDir,
		Error:    task.Error,
		Finished: finishedAt,
	})
}

func (d *ytdlpDownloader) doDownload(task *taskState) (string, error) {
	tempDir := filepath.Join(os.TempDir(), "mediabin-downloads", task.ID)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	outputTemplate := filepath.Join(tempDir, "video.%(ext)s")

	args := []string{
		"--output", outputTemplate,
		"--format", "best",
		"--no-playlist",
		"--write-thumbnail",
		"--write-info-json",
		"--no-warnings",
		"--add-headers", "User-Agent:Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		"--progress", "--newline", "--progress-delta", "0.5",
		"--progress-template", "download:%(progress)j",
		"--quiet",
		"--verbose",
		task.URL,
	}

	cmd := exec.CommandContext(d.ctx, GetYtdlpPath(), args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("yt-dlp process failed: %w", err)
	}

	// Progress lines go to stdout for regular downloads and stderr for HLS.
	// Parse both streams; use a WaitGroup to close progressCh only when both are done.
	progressCh := make(chan []byte)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); d.parseLines(stdout, progressCh) }()
	go func() { defer wg.Done(); d.parseLines(io.TeeReader(stderr, d.stderrWriter), progressCh) }()
	go func() { wg.Wait(); close(progressCh) }()

	parser := NewYTDLPParser(progressCh)
	for progress := range parser.Parse() {
		task.Progress = &Progress{
			Percent:         progress.Percent,
			Speed:           strings.TrimSpace(progress.SpeedStr),
			Eta:             strings.TrimSpace(progress.EtaStr),
			TotalBytes:      progress.TotalBytes,
			DownloadedBytes: progress.DownloadedBytes,
			Filename:        progress.Filename,
		}

		d.emitEvent(DownloadEvent{
			TaskID:   task.ID,
			Status:   StatusDownloading,
			Progress: task.Progress,
		})
	}

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("yt-dlp failed: %w", err)
	}

	infoJSONPath := filepath.Join(tempDir, "video.info.json")
	metadata, err := loadVideoMetadata(infoJSONPath)
	if err != nil {
		return "", fmt.Errorf("failed to load video metadata: %w", err)
	}

	task.Metadata = metadata

	d.emitEvent(DownloadEvent{
		TaskID:   task.ID,
		Status:   StatusDownloading,
		Metadata: metadata,
	})

	return tempDir, nil
}

func (d *ytdlpDownloader) emitEvent(event DownloadEvent) {
	d.mu.RLock()
	subscribers := make([]chan DownloadEvent, 0, len(d.subscribers))
	for ch := range d.subscribers {
		subscribers = append(subscribers, ch)
	}
	d.mu.RUnlock()

	for _, ch := range subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

func (d *ytdlpDownloader) parseLines(r io.Reader, progressCh chan []byte) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		progressCh <- cp
	}
}

func loadVideoMetadata(path string) (*VideoMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read info.json: %w", err)
	}

	var metadata VideoMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse info.json: %w", err)
	}

	enrichMetadata(&metadata)
	return &metadata, nil
}

type YTDLPProgress struct {
	Status          string  `json:"status"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	TotalBytes      int64   `json:"total_bytes"`
	Percent         float64 `json:"_percent"`
	SpeedStr        string  `json:"_speed_str"`
	EtaStr          string  `json:"_eta_str"`
	Filename        string  `json:"filename"`
}

type YTDLPParser struct {
	input chan []byte
}

func NewYTDLPParser(input chan []byte) *YTDLPParser {
	return &YTDLPParser{input: input}
}

func (p *YTDLPParser) Parse() chan *YTDLPProgress {
	output := make(chan *YTDLPProgress)

	go func() {
		defer close(output)

		for line := range p.input {
			if len(line) == 0 || line[0] != '{' {
				continue
			}
			var progress YTDLPProgress
			if err := json.Unmarshal(line, &progress); err == nil && progress.Status == "downloading" {
				output <- &progress
			}
		}
	}()

	return output
}

func GetYtdlpPath() string {
	if path := os.Getenv("YT_DLP_PATH"); path != "" {
		return path
	}
	return "yt-dlp"
}

func (d *ytdlpDownloader) GetActiveTasks() []TaskSnapshot {
	var snapshots []TaskSnapshot
	d.activeTasks.Range(func(_, value any) bool {
		task := value.(*taskState)
		snap := TaskSnapshot{
			ID:     task.ID,
			Status: task.Status,
		}
		if task.Metadata != nil {
			snap.Title = task.Metadata.Title
		}
		if task.Progress != nil {
			snap.Percent = task.Progress.Percent
			snap.Speed = task.Progress.Speed
			snap.Eta = task.Progress.Eta
		}
		snapshots = append(snapshots, snap)
		return true
	})
	return snapshots
}

func (d *ytdlpDownloader) Subscribe() chan DownloadEvent {
	ch := make(chan DownloadEvent, 64)
	d.mu.Lock()
	d.subscribers[ch] = true
	d.mu.Unlock()
	return ch
}

func (d *ytdlpDownloader) Unsubscribe(ch chan DownloadEvent) {
	d.mu.Lock()
	delete(d.subscribers, ch)
	d.mu.Unlock()
}
