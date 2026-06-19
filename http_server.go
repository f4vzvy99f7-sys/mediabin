package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	agefs "github.com/MaxDillon/age-filestore/store"
	"github.com/MaxDillon/mediabin-golang/web"
)

const maxVideoChunk = 1 << 20 // 1 MB, matches Python implementation

type apiServer struct {
	ledger *Ledger
	store  agefs.Store
	logger *log.Logger
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

	// Stat first to get the principle filename (needed for Content-Type).
	he, err := s.store.Stat(id)
	if err != nil || he.Principle == nil {
		http.NotFound(w, r)
		return
	}

	rc, size, err := s.store.Open(id, he.Principle.Name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()

	start, requestedEnd := parseRangeHeader(r, size)
	cappedEnd := min(start+maxVideoChunk-1, requestedEnd, size-1)

	req := r.Clone(r.Context())
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, cappedEnd))
	http.ServeContent(w, req, he.Principle.Name, time.Time{}, rc)
}

// parseRangeHeader extracts the start and end byte positions from a Range header.
// If the header is absent or malformed, it returns 0 and fileSize-1 (full file).
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

func startHTTPServer(ctx context.Context, ledger *Ledger, s agefs.Store, port string, logger *log.Logger) {
	srv := &apiServer{ledger: ledger, store: s, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/media/list", srv.handleListMedia)
	mux.HandleFunc("GET /api/media/play/{id}", srv.handlePlayMedia)
	mux.HandleFunc("GET /web", func(w http.ResponseWriter, r *http.Request) {
		var videos []web.VideoData
		for _, media := range ledger.entries {
			videos = append(videos, web.VideoData{
				Id:    media.ID,
				Title: media.Title,
				Tags:  media.Tags,
			})
		}
		web.Videos(videos).Render(r.Context(), w)
	})

	addr := fmt.Sprintf(":%s", port)
	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background()) //nolint:errcheck
	}()

	go func() {
		logger.Printf("HTTP API listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("HTTP server error: %v", err)
		}
	}()
}
