package main

import (
	"context"
	"io"
	"testing"
	"time"
)

// testLogWriter forwards writes to t.Log, useful for capturing yt-dlp stderr.
type testLogWriter struct {
	t      *testing.T
	prefix string
}

func (w *testLogWriter) Write(p []byte) (n int, err error) {
	w.t.Logf("%s%s", w.prefix, string(p))
	return len(p), nil
}

func TestDownloaderFetchInfo(t *testing.T) {
	downloader := NewDownloader(context.Background(), io.Discard)
	ctx := context.Background()

	metadata, err := downloader.FetchInfo(ctx, "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("FetchInfo failed: %v", err)
	}

	if metadata.ID != "dQw4w9WgXcQ" {
		t.Errorf("Expected ID 'dQw4w9WgXcQ', got '%s'", metadata.ID)
	}

	if metadata.Title == "" {
		t.Error("Expected non-empty title")
	}

	t.Logf("Fetched metadata: %+v", metadata)
}

func TestDownloaderStartDownload(t *testing.T) {
	downloader := NewDownloader(context.Background(), io.Discard)
	ctx := t.Context()

	eventCh := downloader.Subscribe()
	defer downloader.Unsubscribe(eventCh)

	err := downloader.StartDownload(ctx, "test-task-1", "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	var startedEvent DownloadEvent
	var progressEvents []DownloadEvent
	var finishedEvent DownloadEvent

	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-timeout:
			t.Fatal("Test timed out waiting for download to complete")
		case event := <-eventCh:
			switch event.Status {
			case StatusDownloading:
				if startedEvent.TaskID == "" {
					startedEvent = event
					t.Logf("Download started: %s", event.TaskID)
				} else {
					progressEvents = append(progressEvents, event)
				}
			case StatusFinished:
				finishedEvent = event
				t.Logf("Download finished: %s, temp dir: %s", event.TaskID, event.TempDir)
			case StatusError:
				t.Fatalf("Download failed: %v", event.Error)
			case StatusCancelled:
				t.Fatal("Download was cancelled")
			}

			if finishedEvent.TaskID != "" {
				goto done
			}
		}
	}

done:
	if startedEvent.TaskID == "" {
		t.Error("Never received started event")
	}

	if len(progressEvents) == 0 {
		t.Log("No progress events received (video may be small)")
	} else {
		t.Logf("Received %d progress updates", len(progressEvents))
	}

	if finishedEvent.TaskID == "" {
		t.Error("Never received finished event")
	}

	if finishedEvent.TempDir == "" {
		t.Error("Finished event missing temp directory")
	}

	if finishedEvent.Metadata == nil {
		t.Error("Finished event missing metadata")
	}

	if finishedEvent.Metadata.ID != "dQw4w9WgXcQ" {
		t.Errorf("Expected metadata ID 'dQw4w9WgXcQ', got '%s'", finishedEvent.Metadata.ID)
	}

	t.Logf("Final metadata: %+v", finishedEvent.Metadata)
}

func TestDownloaderNoSubscriber(t *testing.T) {
	downloader := NewDownloader(context.Background(), io.Discard)
	ctx := context.Background()

	err := downloader.StartDownload(ctx, "task-1", "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	if err != ErrNoSubscriber {
		t.Errorf("Expected ErrNoSubscriber, got: %v", err)
	}

	t.Log("No-subscriber check correctly enforced")
}

func TestDownloaderDuplicate(t *testing.T) {
	downloader := NewDownloader(context.Background(), io.Discard)
	ctx := context.Background()

	eventCh := downloader.Subscribe()
	defer downloader.Unsubscribe(eventCh)

	err := downloader.StartDownload(ctx, "dup-test", "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("First StartDownload failed: %v", err)
	}

	err = downloader.StartDownload(ctx, "dup-test", "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	if err != ErrAlreadyDownloading {
		t.Errorf("Expected ErrAlreadyDownloading, got: %v", err)
	}

	t.Log("Duplicate detection correctly enforced")
}

// TestDownloaderProgressParsing verifies that progress events are correctly parsed
// during a real download. It cancels after 60 seconds so the test doesn't run forever.
// Run with: go test -v -run TestDownloaderProgressParsing -timeout 90s
func TestDownloaderProgressParsing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stderr := &testLogWriter{t: t, prefix: "[yt-dlp stderr] "}
	downloader := NewDownloader(ctx, stderr)

	eventCh := downloader.Subscribe()
	defer downloader.Unsubscribe(eventCh)

	const url = "https://www.youtube.com/watch?v=b2nfKYLrmBk"
	err := downloader.StartDownload(ctx, "progress-test", url)
	if err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	var progressCount int
	var lastProgress *Progress

	for {
		select {
		case event, ok := <-eventCh:
			if !ok {
				t.Log("Event channel closed")
				goto done
			}
			switch event.Status {
			case StatusDownloading:
				if event.Progress != nil {
					progressCount++
					lastProgress = event.Progress
					t.Logf("[progress #%d] %.1f%% speed=%s eta=%s downloaded=%d total=%d file=%s",
						progressCount,
						event.Progress.Percent,
						event.Progress.Speed,
						event.Progress.Eta,
						event.Progress.DownloadedBytes,
						event.Progress.TotalBytes,
						event.Progress.Filename,
					)
				}
			case StatusFinished:
				t.Logf("Download finished unexpectedly early (small video?): tempDir=%s", event.TempDir)
				goto done
			case StatusError:
				// A context-cancelled error is expected here since we cancel after 60s
				if ctx.Err() != nil {
					t.Logf("Download cancelled after timeout (expected): %v", event.Error)
				} else {
					t.Errorf("Download failed unexpectedly: %v", event.Error)
				}
				goto done
			case StatusCancelled:
				t.Log("Download cancelled (expected after timeout)")
				goto done
			}
		case <-ctx.Done():
			t.Log("Context deadline reached, stopping event loop")
			goto done
		}
	}

done:
	if progressCount == 0 {
		t.Error("No progress events received — progress parsing may be broken")
	} else {
		t.Logf("Received %d progress events total", progressCount)
	}

	if lastProgress != nil {
		t.Logf("Last progress: %.1f%% speed=%s eta=%s downloaded=%d total=%d",
			lastProgress.Percent,
			lastProgress.Speed,
			lastProgress.Eta,
			lastProgress.DownloadedBytes,
			lastProgress.TotalBytes,
		)
	}
}
