// Package api serves the REST API and the embedded web app.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/tamasmagyar/sscale/internal/app"
	"github.com/tamasmagyar/sscale/internal/db"
	"github.com/tamasmagyar/sscale/internal/store"
)

// Server is the HTTP API.
type Server struct {
	App *app.App
	// State returns the daemon state (state.json).
	State func() store.State
	// Running reports whether the BLE daemon is running.
	Running func() bool
	UI      fs.FS // built web app; nil disables it
	Log     *slog.Logger
	// KeyOverride, when set, is used instead of the stored API key.
	KeyOverride string
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.Handle("GET /api/status", s.auth(s.status))
	mux.Handle("GET /api/measurements", s.auth(s.listMeasurements))
	mux.Handle("PATCH /api/measurements/{id}", s.auth(s.patchMeasurement))
	mux.Handle("DELETE /api/measurements/{id}", s.auth(s.deleteMeasurement))
	mux.Handle("GET /api/settings", s.auth(s.getSettings))
	mux.Handle("PUT /api/settings", s.auth(s.putSettings))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	if s.UI != nil {
		mux.Handle("/", spa(s.UI))
	}
	return logRequests(s.Log, securityHeaders(mux))
}

// ------------------------------------------------------------------ auth

func (s *Server) key(ctx context.Context) (string, error) {
	if s.KeyOverride != "" {
		return s.KeyOverride, nil
	}
	k, _, err := s.App.DB.APIKey(ctx)
	return k, err
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-API-Key")
		if a := r.Header.Get("Authorization"); got == "" && strings.HasPrefix(a, "Bearer ") {
			got = strings.TrimPrefix(a, "Bearer ")
		}
		want, err := s.key(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "api key unavailable")
			return
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			time.Sleep(300 * time.Millisecond) // slow down guessing
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		h(w, r)
	})
}

// ------------------------------------------------------------------ handlers

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st := s.State()
	total, valid, err := s.App.DB.Count(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	latest, _ := s.App.Latest(r.Context())
	running := s.Running == nil || s.Running()
	writeJSON(w, http.StatusOK, map[string]any{
		"daemon": map[string]any{
			"running":    running,
			"phase":      st.Daemon.Phase,
			"started_at": st.Daemon.StartedAt,
		},
		"scale": map[string]any{
			"address":   st.Scale.Address,
			"name":      st.Scale.Name,
			"awake":     running && st.Scale.Awake,
			"last_seen": st.Scale.LastSeen,
			"rssi":      st.Scale.RSSI,
		},
		"paired":       st.Pairing.Paired,
		"device":       st.Pairing.Device,
		"last_session": st.LastSession,
		"counts":       map[string]int{"total": total, "valid": valid},
		"latest":       latest,
	})
}

func parseBound(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02", v, time.Local)
}

func (s *Server) listMeasurements(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err := parseBound(q.Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from: use YYYY-MM-DD or RFC 3339")
		return
	}
	to, err := parseBound(q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to: use YYYY-MM-DD or RFC 3339")
		return
	}
	all := q.Get("include_invalid") == "true" || q.Get("include_invalid") == "1"
	views, err := s.App.List(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]app.View, 0, len(views))
	for _, v := range views {
		if all || v.Valid {
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) patchMeasurement(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		// true = mine, false = not mine, null = let the filter decide
		Valid *bool `json:"valid"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.App.SetManual(r.Context(), id, body.Valid)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "measurement not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteMeasurement(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	err := s.App.Delete(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "measurement not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.App.Settings(r.Context()))
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var in db.Settings
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.App.UpdateSettings(r.Context(), in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.App.Settings(r.Context()))
}

// ------------------------------------------------------------------ helpers

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }

func logRequests(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		start := time.Now()
		h.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.code, "took", time.Since(start).Round(time.Millisecond))
		}
	})
}

// spa serves the built web app, falling back to index.html for client-side
// routes.
func spa(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(ui, p); err != nil {
			p = "index.html"
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		switch {
		case strings.HasPrefix(p, "assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			// index.html, sw.js, manifest: always revalidate so updates land.
			w.Header().Set("Cache-Control", "no-cache")
		}
		if strings.HasSuffix(p, ".webmanifest") {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		files.ServeHTTP(w, r)
	})
}
