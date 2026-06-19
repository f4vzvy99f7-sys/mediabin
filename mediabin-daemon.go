package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	agefs "github.com/MaxDillon/age-filestore/store"
	"github.com/MaxDillon/daemonizer/daemon"
)

type ProcessInfo struct {
	ID        string
	Title     string
	Status    string
	Percent   float64
	Speed     string
	Eta       string
	IsPending bool // true for queued-but-not-yet-downloading entries
}

type ListCurrentProcsResp struct {
	Processes []ProcessInfo
}

type MediaInfo struct {
	ID    string
	Title string
}

type ListMediaResp struct {
	Media []MediaInfo
}

type DiskUsageResp struct {
	Path       string
	TotalBytes uint64
	UsedBytes  uint64
	FreeBytes  uint64
}

type MediabinDaemon struct {
	RegisterNewDownload func(url string) error
	ListCurrentProcs    func() (ListCurrentProcsResp, error)
	ListMedia           func(title_like string, tags []string) (ListMediaResp, error)
	ListTags            func() ([]string, error)
	DiskUsage           func() (DiskUsageResp, error)
	GetLogs             func() error
	ArchiveLogs         func() (string, error)
}

func InitMediabin() (*MediabinDaemon, error) {

	return daemon.CreateClient[MediabinDaemon]("mediabin-go", func(h *daemon.Handlers) error {
		logger := daemon.Logger()

		passwd, ok := os.LookupEnv("DB_PASSWD")
		if !ok {
			return errors.New("no password")
		}
		datadir, ok := os.LookupEnv("DB_DATADIR")
		if !ok {
			return errors.New("no datadir")
		}

		ledgerpath := path.Join(datadir, "ledger")

		if err := os.MkdirAll(datadir, 0755); err != nil {
			return fmt.Errorf("failed to create datadir: %w", err)
		}

		ledger, err := LoadFromFile(ledgerpath, passwd)
		if err != nil {
			return err
		}

		// Reset any entries left in "downloading" state from a previous daemon run
		// back to "pending" so they get picked up again.
		needsSync := false
		for _, entry := range ledger.List() {
			if entry.Status == "downloading" {
				entry.Status = "pending"
				ledger.Update(entry)
				needsSync = true
				logger.Printf("reset interrupted download to pending: %s", entry.ID)
			}
		}
		if needsSync {
			if err := ledger.SyncToFile(ledgerpath); err != nil {
				return fmt.Errorf("failed to sync ledger after restart reset: %w", err)
			}
		}

		identity, err := age.NewScryptIdentity(passwd)
		if err != nil {
			return fmt.Errorf("failed to create age identity: %w", err)
		}
		mss, err := agefs.New(agefs.Config{
			Root:       datadir,
			Identities: []age.Identity{identity},
		})
		if err != nil {
			return fmt.Errorf("failed to open media store: %w", err)
		}

		serverCtx, cancelServerCtx := context.WithCancel(context.Background())
		h.OnShutdown(func(ctx context.Context) error {
			cancelServerCtx()
			return nil
		})

		h.OnShutdown(func(ctx context.Context) error {
			logger.Println("shutting down...")
			cancelServerCtx()
			mss.Close() //nolint:errcheck
			if err := ledger.SyncToFile(ledgerpath); err != nil {
				logger.Printf("failed to sync ledger on shutdown: %v", err)
				return err
			}
			return nil
		})

		port := os.Getenv("DB_PORT")
		if port == "" {
			port = "8080"
		}

		downloader := NewDownloader(serverCtx, logger.Writer())
		startHTTPServer(serverCtx, ledger, mss, port, logger)

		go func() {
			c := downloader.Subscribe()
			for event := range c {
				switch event.Status {
				case StatusFinished:
					entry, ok := ledger.Get(event.TaskID)
					if !ok {
						logger.Printf("download finished but entry not found: %s", event.TaskID)
						continue
					}
					if err := storeInAgeFS(datadir, passwd, entry.ID, event.TempDir); err != nil {
						logger.Printf("failed to store download in age-fs: %v", err)
						entry.Status = "error"
						ledger.Update(entry)
						if err := ledger.SyncToFile(ledgerpath); err != nil {
							logger.Printf("failed to sync ledger after store error: %v", err)
						}
						continue
					}
					now := time.Now()
					entry.Status = "complete"
					entry.TimestampInstalled = &now
					ledger.Update(entry)
					if err := ledger.SyncToFile(ledgerpath); err != nil {
						logger.Printf("failed to sync ledger after completion: %v", err)
					}
					logger.Printf("download completed and encrypted: %s", event.TaskID)

				case StatusError:
					entry, ok := ledger.Get(event.TaskID)
					if ok {
						entry.Status = "error"
						ledger.Update(entry)
						if err := ledger.SyncToFile(ledgerpath); err != nil {
							logger.Printf("failed to sync ledger after error: %v", err)
						}
						logger.Printf("download error: %s - %v", event.TaskID, event.Error)
					}
				case StatusCancelled:
					entry, ok := ledger.Get(event.TaskID)
					if ok {
						entry.Status = "error"
						ledger.Update(entry)
						if err := ledger.SyncToFile(ledgerpath); err != nil {
							logger.Printf("failed to sync ledger after cancel: %v", err)
						}
						logger.Printf("download cancelled: %s", event.TaskID)
					}
				}
			}
		}()

		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-serverCtx.Done():
					return
				case <-ticker.C:
					entries := ledger.List()
					for _, entry := range entries {
						if entry.Status == "pending" {
							if err := downloader.StartDownload(serverCtx, entry.ID, entry.OriginUrl); err != nil {
								if errors.Is(err, ErrAlreadyDownloading) {
									// Worker already has this task; nothing to do.
									continue
								}
								logger.Printf("failed to start download %s: %v", entry.ID, err)
								entry.Status = "error"
								ledger.Update(entry)
								if err := ledger.SyncToFile(ledgerpath); err != nil {
									logger.Printf("failed to sync ledger after dispatch error: %v", err)
								}
								continue
							}
							// Mark as downloading so the dispatcher ignores it on future ticks.
							entry.Status = "downloading"
							ledger.Update(entry)
							logger.Printf("started download: %s", entry.ID)
						}
					}
				}
			}
		}()

		h.Handle("RegisterNewDownload", func(ctx context.Context, url string) error {
			stdout := daemon.Stdout(ctx)
			info, err := downloader.FetchInfo(ctx, url)
			if err != nil {
				logger.Printf("error fetching info: %v", err)
				return err
			}

			// Duplicate check using the stable MD5 identifier.
			if _, exists := ledger.Get(info.MbIdentifier); exists {
				fmt.Fprintf(stdout, "%s is already downloaded or is currently in the queue\n", url)
				return nil
			}

			fmt.Fprintf(stdout, "Queued: %s\n", info.Title)
			logger.Printf("Title: %s\n Url: %s\n", info.Title, info.WebpageURL)

			entry := MediaEntry{
				ID:           info.MbIdentifier,
				Title:        info.Title,
				OriginUrl:    info.WebpageURL,
				VideoUrl:     info.URL,
				ThumbnailUrl: info.Thumbnail,
				ObjectPath:   info.MbPath,
				Status:       "pending",
				Tags:         []string{},
			}

			// Auto-tag uploader as studio and cast as actors, matching Python behaviour.
			if info.Uploader != "" {
				entry.Tags = append(entry.Tags, "studio:"+normalizeTagValue(info.Uploader))
			}
			for _, actor := range info.Cast {
				entry.Tags = append(entry.Tags, "actor:"+normalizeTagValue(actor))
			}

			ledger.Add(entry)
			if err := ledger.SyncToFile(ledgerpath); err != nil {
				logger.Printf("failed to sync ledger after register: %v", err)
				return err
			}
			return nil
		})

		h.Handle("ListCurrentProcs", func(ctx context.Context) (ListCurrentProcsResp, error) {
			// Build a set of IDs that are actively downloading.
			activeTasks := downloader.GetActiveTasks()
			activeByID := make(map[string]TaskSnapshot, len(activeTasks))
			for _, t := range activeTasks {
				activeByID[t.ID] = t
			}

			var procs []ProcessInfo

			// Actively downloading tasks get their live progress.
			// task.Metadata (and thus task.Title) is only populated after the download
			// finishes; during the download itself the title comes from the ledger entry
			// that was created by RegisterNewDownload.
			for _, t := range activeTasks {
				title := t.Title
				if title == "" {
					if entry, ok := ledger.Get(t.ID); ok {
						title = entry.Title
					}
				}
				procs = append(procs, ProcessInfo{
					ID:        t.ID,
					Title:     title,
					Status:    string(t.Status),
					Percent:   t.Percent,
					Speed:     t.Speed,
					Eta:       t.Eta,
					IsPending: false,
				})
			}

			// All pending ledger entries that are not yet in the active set.
			for _, entry := range ledger.List() {
				if entry.Status != "pending" {
					continue
				}
				if _, active := activeByID[entry.ID]; active {
					continue
				}
				procs = append(procs, ProcessInfo{
					ID:        entry.ID,
					Title:     entry.Title,
					IsPending: true,
				})
			}

			return ListCurrentProcsResp{Processes: procs}, nil
		})

		h.Handle("ListMedia", func(ctx context.Context, title_like string, tags []string) (ListMediaResp, error) {
			entries := ledger.List()
			var result []MediaInfo
			title_lower := strings.ToLower(title_like)

			for _, entry := range entries {
				// Only show completed entries, matching Python behaviour.
				if entry.Status != "complete" {
					continue
				}
				if title_like != "" && !strings.Contains(strings.ToLower(entry.Title), title_lower) {
					continue
				}
				if len(tags) > 0 {
					// OR logic: entry must have at least one of the specified tags.
					matched := false
					for _, tag := range tags {
						for _, et := range entry.Tags {
							if et == tag {
								matched = true
								break
							}
						}
						if matched {
							break
						}
					}
					if !matched {
						continue
					}
				}
				result = append(result, MediaInfo{
					ID:    entry.ID,
					Title: entry.Title,
				})
			}

			return ListMediaResp{Media: result}, nil
		})

		h.Handle("ListTags", func(ctx context.Context) ([]string, error) {
			entries := ledger.List()
			tagSet := make(map[string]bool)
			for _, entry := range entries {
				for _, tag := range entry.Tags {
					tagSet[tag] = true
				}
			}
			tags := make([]string, 0, len(tagSet))
			for tag := range tagSet {
				tags = append(tags, tag)
			}
			return tags, nil
		})

		h.Handle("DiskUsage", func(ctx context.Context) (DiskUsageResp, error) {
			var stat syscall.Statfs_t
			if err := syscall.Statfs(datadir, &stat); err != nil {
				return DiskUsageResp{}, fmt.Errorf("statfs failed: %w", err)
			}
			total := stat.Blocks * uint64(stat.Bsize)
			free := stat.Bfree * uint64(stat.Bsize)
			return DiskUsageResp{
				Path:       datadir,
				TotalBytes: total,
				UsedBytes:  total - free,
				FreeBytes:  free,
			}, nil
		})

		h.Handle("GetLogs", func(ctx context.Context) error {
			path := daemon.LogPath()
			if path == "" {
				return errors.New("log path unavailable")
			}
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("failed to open log file: %w", err)
			}
			defer f.Close()
			_, err = io.Copy(daemon.Stdout(ctx), f)
			return err
		})

		h.Handle("ArchiveLogs", func(ctx context.Context) (string, error) {
			return daemon.ArchiveLog()
		})

		return nil
	})
}

// normalizeTagValue lower-cases a string and replaces spaces with underscores,
// matching Python's tag normalization: uploader.lower().replace(' ', '_').
func normalizeTagValue(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", "_"))
}

// storeInAgeFS encrypts all files from a completed yt-dlp download directory
// into the age-filesystem archive for the given hash, then removes the temp dir.
//
// File mapping:
//   - video.info.json  → meta.json  (sidecar)
//   - video.{video ext} → principle  (auto-detected MIME → principle.{ext})
//   - everything else  → thumbnail.{ext} (sidecar)
func storeInAgeFS(datadir, passphrase, hash, tempDir string) error {
	des, err := os.ReadDir(tempDir)
	if err != nil {
		return fmt.Errorf("read temp dir: %w", err)
	}

	// Open all files before calling Put so they are all readable during the
	// single encrypt pass.
	type openedFile struct {
		f    *os.File
		name string // archive name ("" = principle auto-detect)
	}

	opened := make([]openedFile, 0, len(des))
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		src := filepath.Join(tempDir, de.Name())
		f, err := os.Open(src)
		if err != nil {
			for _, o := range opened {
				o.f.Close()
			}
			return fmt.Errorf("open %s: %w", de.Name(), err)
		}
		var archiveName string
		switch {
		case de.Name() == "video.info.json":
			archiveName = "meta.json"
		case isVideoExt(strings.ToLower(filepath.Ext(de.Name()))):
			archiveName = "" // principle — MIME auto-detected by store.Put
		default:
			archiveName = "thumbnail" + strings.ToLower(filepath.Ext(de.Name()))
		}
		opened = append(opened, openedFile{f: f, name: archiveName})
	}

	putFiles := make([]agefs.PutFile, len(opened))
	for i, o := range opened {
		putFiles[i] = agefs.PutFile{Name: o.name, Reader: o.f}
	}

	putErr := agefs.Put(datadir, hash, passphrase, putFiles)

	for _, o := range opened {
		o.f.Close()
	}

	if putErr != nil {
		return putErr
	}

	os.RemoveAll(tempDir)
	return nil
}

func isVideoExt(ext string) bool {
	switch ext {
	case ".mp4", ".mkv", ".webm", ".avi", ".mov", ".m4v":
		return true
	}
	return false
}
