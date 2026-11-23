package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/selector.html
var selectorHTML []byte

// selectorContext is what the "Select correct anime" page shows on open.
type selectorContext struct {
	Query        string
	File         string
	CurrentID    int
	CurrentTitle string
	CurrentCover string
}

type selectorRow struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	AltTitle string `json:"altTitle,omitempty"`
	Year     int    `json:"year,omitempty"`
	Format   string `json:"format,omitempty"`
	Episodes int    `json:"episodes,omitempty"`
	Cover    string `json:"cover,omitempty"`
}

func selectorRowFrom(m mediaLite) selectorRow {
	title := mediaLiteTitle(m)
	alt := strings.TrimSpace(m.Title.Romaji)
	if strings.EqualFold(alt, title) {
		alt = ""
	}
	return selectorRow{
		ID:       m.ID,
		Title:    title,
		AltTitle: alt,
		Year:     m.StartDate.Year,
		Format:   m.Format,
		Episodes: m.Episodes,
		Cover:    firstNonEmpty(m.CoverImage.Medium, m.CoverImage.Large),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// startAniListSelector serves a small local page for picking the right
// AniList entry and opens it in the browser. onSelect is called with the
// chosen media ID. The server shuts down shortly after a pick, or after 10
// minutes of no use.
func startAniListSelector(sel selectorContext, ua string, onSelect func(id int)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	token := randBase64URL(18)

	var (
		cacheMu sync.Mutex
		cache   = map[string][]selectorRow{} // lower-cased query -> results
	)
	done := make(chan struct{}, 1)
	activity := make(chan struct{}, 1)
	touch := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}

	// authorized checks the per-session token so other websites can't drive
	// this local server.
	authorized := func(r *http.Request) bool {
		got := r.Header.Get("X-Koushin-Token")
		return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		touch()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(selectorHTML)
	})

	mux.HandleFunc("/api/context", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
			return
		}
		var current any
		if sel.CurrentID > 0 {
			current = selectorRow{ID: sel.CurrentID, Title: firstNonEmpty(sel.CurrentTitle, "AniList #"+strconv.Itoa(sel.CurrentID)), Cover: sel.CurrentCover}
		}
		writeJSON(w, http.StatusOK, map[string]any{"query": sel.Query, "file": sel.File, "current": current})
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
			return
		}
		touch()
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if len([]rune(q)) < 2 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Type at least 2 characters."})
			return
		}
		key := strings.ToLower(q)

		cacheMu.Lock()
		rows, ok := cache[key]
		cacheMu.Unlock()
		if ok {
			writeJSON(w, http.StatusOK, map[string]any{"results": rows})
			return
		}

		if wait := anilistCooldown(); wait > 0 {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":      "AniList rate limit reached.",
				"retryAfter": int(math.Ceil(wait.Seconds())),
			})
			return
		}

		// The request context is cancelled if the page aborts a stale search.
		c, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		ms, err := searchAniList(c, q, ua)
		if err != nil {
			if errors.Is(err, errAniListRateLimited) {
				writeJSON(w, http.StatusTooManyRequests, map[string]any{
					"error":      "AniList rate limit reached.",
					"retryAfter": max(1, int(math.Ceil(anilistCooldown().Seconds()))),
				})
				return
			}
			if r.Context().Err() != nil {
				return // the page gave up on this search
			}
			logAppend("selector: search failed: ", err.Error())
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Could not reach AniList. Try again in a moment."})
			return
		}

		rows = make([]selectorRow, 0, len(ms))
		for _, m := range ms {
			rows = append(rows, selectorRowFrom(m))
		}
		cacheMu.Lock()
		cache[key] = rows
		cacheMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"results": rows})
	})

	mux.HandleFunc("/api/select", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorized(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
			return
		}
		var body struct {
			ID int `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
			return
		}
		if onSelect != nil {
			onSelect(body.ID)
		}
		select {
		case done <- struct{}{}:
		default:
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	go func() {
		idle := time.NewTimer(10 * time.Minute)
		defer idle.Stop()
	wait:
		for {
			select {
			case <-done:
				time.Sleep(5 * time.Second) // let the page finish loading its success state
				break wait
			case <-activity:
				idle.Reset(10 * time.Minute) // Go 1.23+ timers need no draining
			case <-idle.C:
				break wait
			}
		}
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c)
		cancel()
	}()

	// The token travels in the URL fragment, which browsers never send to servers.
	page := "http://" + ln.Addr().String() + "/#t=" + url.QueryEscape(token)
	openBrowser(page)
	return nil
}
