package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/f4vzvy99f7-sys/daemonizer"
	vault "github.com/f4vzvy99f7-sys/vaultblob-go"
)

//go:embed www
var staticFS embed.FS

const maxVideoChunk = 1 << 20 // 1 MB, matches Python implementation

type apiServer struct {
	ledger  *Ledger
	session *vault.Session
	logger  *daemonizer.Logger
}

type mediaListItem struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
}

func (s *apiServer) handleListMedia(w http.ResponseWriter, r *http.Request) {
	tagParam := r.URL.Query().Get("tag")
	q := r.URL.Query().Get("q")

	var filterTags []string
	if tagParam != "" {
		filterTags = []string{tagParam}
	}

	entries := s.ledger.List()
	items := make([]mediaListItem, 0)
	qLower := strings.ToLower(q)

	for _, entry := range entries {
		if entry.Status != "complete" {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(entry.Title), qLower) {
			continue
		}
		if len(filterTags) > 0 {
			matched := false
			for _, ft := range filterTags {
				if slices.Contains(entry.Tags, ft) {
					matched = true
				}
				if matched {
					break
				}
			}
			if !matched {
				continue
			}
		}
		tags := entry.Tags
		if tags == nil {
			tags = []string{}
		}
		items = append(items, mediaListItem{
			ID:    entry.ID,
			Title: entry.Title,
			Tags:  tags,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=30, Cache-Control: public, max-age=30, stale-while-revalidate=120")
	json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func (s *apiServer) handlePlayMedia(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entry, ok := s.ledger.Get(id)
	if !ok || entry.Status != "complete" {
		http.NotFound(w, r)
		return
	}

	if entry.ObjectPath == "" {
		http.Error(w, "no principle file recorded", http.StatusNotFound)
		return
	}

	fileID := entry.ObjectPath

	size, err := s.session.FileSize(fileID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	rc := s.session.NewFileReadSeeker(fileID)

	start, requestedEnd := parseRangeHeader(r, int64(size))
	cappedEnd := min(start+maxVideoChunk-1, requestedEnd, int64(size)-1)

	req := r.Clone(r.Context())
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, cappedEnd))
	http.ServeContent(w, req, fileID, time.Time{}, rc)
}

func parseRangeHeader(r *http.Request, fileSize int64) (start, end int64) {
	end = fileSize - 1
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		return
	}
	after, ok := strings.CutPrefix(rangeHeader, "bytes=")
	if !ok {
		return
	}
	parts := strings.SplitN(after, "-", 2)
	if len(parts) != 2 {
		return
	}
	if n, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
		start = n
	}
	if parts[1] != "" {
		if n, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			end = n
		}
	}
	return
}

func startHTTPServer(ctx context.Context, ledger *Ledger, session *vault.Session, port string, logger *daemonizer.Logger) {
	srv := &apiServer{ledger: ledger, session: session, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/media/list", srv.handleListMedia)
	mux.HandleFunc("GET /api/media/play/{id}", srv.handlePlayMedia)
	webRoot, _ := fs.Sub(staticFS, "www")
	mux.Handle("GET /", http.FileServer(http.FS(webRoot)))

	addr := fmt.Sprintf(":%s", port)
	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background()) //nolint:errcheck
	}()

	go func() {
		logger.Infof("HTTP API listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Infof("HTTP server error: %v", err)
		}
	}()
}
