package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SAMARTHJAISWAL/linkly/internal/analytics"
	"github.com/SAMARTHJAISWAL/linkly/internal/service"
	"github.com/SAMARTHJAISWAL/linkly/internal/store"
	qrcode "github.com/skip2/go-qrcode"
)

const maxBodyBytes = 8 << 10 // a create request is a URL plus two short fields

// directReferrer labels clicks with no usable Referer. Parentheses cannot
// occur in a hostname, so it never collides with a real host.
const directReferrer = "(direct)"

type createRequest struct {
	URL       string     `json:"url"`
	Alias     string     `json:"alias,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type linkResponse struct {
	Code      string     `json:"code"`
	ShortURL  string     `json:"short_url"`
	URL       string     `json:"url"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Expired   bool       `json:"expired"`
	QRURL     string     `json:"qr_url"`
}

type dayResponse struct {
	Date   string `json:"date"`
	Clicks int64  `json:"clicks"`
}

type referrerResponse struct {
	Host   string `json:"host"`
	Clicks int64  `json:"clicks"`
}

type statsResponse struct {
	linkResponse
	TotalClicks  int64              `json:"total_clicks"`
	Daily        []dayResponse      `json:"clicks_by_day"`
	TopReferrers []referrerResponse `json:"top_referrers"`
}

type errorResponse struct {
	Error     string `json:"error"`
	Field     string `json:"field,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func (s *Server) toResponse(l store.Link) linkResponse {
	resp := linkResponse{
		Code:      l.Code,
		ShortURL:  s.baseURL + "/" + l.Code,
		URL:       l.URL,
		CreatedAt: l.CreatedAt,
		Expired:   s.svc.Expired(l),
		QRURL:     s.baseURL + "/api/links/" + l.Code + "/qr.png",
	}
	if !l.ExpiresAt.IsZero() {
		exp := l.ExpiresAt
		resp.ExpiresAt = &exp
	}
	return resp
}

// handleCreate implements POST /api/links.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, r, http.StatusUnsupportedMediaType, "Content-Type must be application/json", "")
		return
	}
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err == nil {
		// Exactly one value: the next read must hit EOF. Decoder.More is not
		// enough, as it reports false for a stray '}' or ']'.
		if err = dec.Decode(&struct{}{}); err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "request body too large", "")
			return
		}
		writeError(w, r, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "")
		return
	}

	in := service.CreateRequest{URL: req.URL, Alias: req.Alias, RequestHost: r.Host}
	if req.ExpiresAt != nil {
		// The service uses the zero time for "never"; an explicit
		// 0001-01-01T00:00:00Z is a date in the past, not a request for a
		// link that never expires (omit the field or send null for that).
		if req.ExpiresAt.IsZero() {
			writeError(w, r, http.StatusBadRequest, "expires_at must be in the future", "expires_at")
			return
		}
		in.ExpiresAt = *req.ExpiresAt
	}
	link, err := s.svc.Create(r.Context(), in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	resp := s.toResponse(link)
	w.Header().Set("Location", "/api/links/"+link.Code)
	writeJSON(w, http.StatusCreated, resp)
}

// isJSONContentType reports whether a Content-Type header names exactly
// application/json (case-insensitively, parameters allowed). An absent
// header is accepted for clients that do not set one.
func isJSONContentType(ct string) bool {
	if ct == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// handleRedirect implements GET /{code}. It is the hot path: one indexed
// read, then the click is handed off to the recorder without waiting.
func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() { s.m.redirectLatency.Observe(time.Since(start).Seconds()) }()

	code := r.PathValue("code")
	link, err := s.svc.Resolve(r.Context(), code)
	switch {
	case errors.Is(err, service.ErrNotFound):
		http.Error(w, "short link not found", http.StatusNotFound)
		return
	case errors.Is(err, service.ErrExpired):
		http.Error(w, "short link has expired", http.StatusGone)
		return
	case err != nil:
		s.log.ErrorContext(r.Context(), "resolving link", "code", code, "err", err, "request_id", RequestID(r.Context()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// A target on this very server (by the name the client used or the
	// configured one) would loop; refuse it rather than count every lap.
	if s.svc.PointsAtSelf(link.URL, r.Host) {
		s.m.redirectLoops.Inc()
		http.Error(w, "short link points back at this server", http.StatusLoopDetected)
		return
	}

	// HEAD requests (link checkers, some unfurlers) are answered but are
	// not clicks.
	if r.Method == http.MethodGet {
		s.clicks.Record(analytics.Click{
			Code:    link.Code,
			At:      s.now().UTC(),
			RefHost: analytics.RefererHost(r.Referer()),
		})
	}
	// Every click must reach the server to be counted, so the redirect must
	// not be cached.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", link.URL)
	w.WriteHeader(http.StatusFound)
}

// handleStats implements GET /api/links/{code}.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.Stats(r.Context(), r.PathValue("code"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	resp := statsResponse{
		linkResponse: s.toResponse(st.Link),
		TotalClicks:  st.Total,
		Daily:        make([]dayResponse, len(st.Daily)),
		TopReferrers: make([]referrerResponse, len(st.Referrers)),
	}
	for i, d := range st.Daily {
		resp.Daily[i] = dayResponse{Date: d.Day, Clicks: d.Clicks}
	}
	for i, ref := range st.Referrers {
		host := ref.Host
		if host == "" {
			host = directReferrer
		}
		resp.TopReferrers[i] = referrerResponse{Host: host, Clicks: ref.Clicks}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

const (
	qrDefaultSize = 256
	qrMinSize     = 64
	qrMaxSize     = 1024
)

// handleQR implements GET /api/links/{code}/qr.png?size=N.
func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	size := qrDefaultSize
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < qrMinSize || n > qrMaxSize {
			writeError(w, r, http.StatusBadRequest, "size must be an integer from 64 to 1024", "size")
			return
		}
		size = n
	}
	link, err := s.svc.Resolve(r.Context(), r.PathValue("code"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	png, err := qrcode.Encode(s.baseURL+"/"+link.Code, qrcode.Medium, size)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	// The image depends only on the code, but the link may be deleted or
	// expire, so keep caching short.
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// handleDelete implements DELETE /api/links/{code}.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if s.adminToken == "" {
		writeError(w, r, http.StatusForbidden, "deletion is disabled: LINKLY_ADMIN_TOKEN is not set", "")
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="linkly"`)
		writeError(w, r, http.StatusUnauthorized, "missing or invalid admin token", "")
		return
	}
	if err := s.svc.Delete(r.Context(), r.PathValue("code")); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorized checks the bearer token in constant time. Both sides are
// hashed first so the comparison also leaks nothing about the length.
func (s *Server) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(h[len(prefix):]))
	want := sha256.Sum256([]byte(s.adminToken))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// handleHealth implements GET /healthz.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.health != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.health.Ping(ctx); err != nil {
			s.log.ErrorContext(r.Context(), "health check failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, r, http.StatusBadRequest, ve.Error(), ve.Field)
	case errors.Is(err, service.ErrAliasTaken):
		writeError(w, r, http.StatusConflict, err.Error(), "alias")
	case errors.Is(err, service.ErrNotFound):
		writeError(w, r, http.StatusNotFound, err.Error(), "")
	case errors.Is(err, service.ErrExpired):
		writeError(w, r, http.StatusGone, err.Error(), "")
	case errors.Is(err, context.Canceled):
		// The client went away; nobody will read a response.
	default:
		s.log.ErrorContext(r.Context(), "request failed", "err", err, "request_id", RequestID(r.Context()))
		writeError(w, r, http.StatusInternalServerError, "internal error", "")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // URLs contain '&'; this is an API, not HTML
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, msg, field string) {
	writeJSON(w, status, errorResponse{Error: msg, Field: field, RequestID: RequestID(r.Context())})
}
