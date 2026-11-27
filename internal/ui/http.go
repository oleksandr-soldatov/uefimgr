package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Server exposes the API over loopback HTTP for running the page in a
// browser: the development mode, and the fallback when no embedded web view
// is available. Every API request needs the per-run token.
type Server struct {
	api      *API
	token    string
	lastSeen atomic.Int64
}

func NewServer(api *API) (*Server, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	s := &Server{api: api, token: hex.EncodeToString(b[:])}
	s.lastSeen.Store(time.Now().UnixNano())
	return s, nil
}

func (s *Server) Token() string { return s.token }

// LastSeen is when the page last called the API. The page pings while it is
// open, so a stale value means it was closed.
func (s *Server) LastSeen() time.Time { return time.Unix(0, s.lastSeen.Load()) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/", s.call)
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	io.WriteString(w, IndexHTML)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) call(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string) { writeJSON(w, code, map[string]string{"error": msg}) }
	// The Host check stops a web page from reaching this server through a
	// DNS name that resolves to loopback.
	if !isLoopback(r.Host) {
		fail(http.StatusForbidden, "uefimgr only accepts loopback requests")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Uefimgr-Token")), []byte(s.token)) != 1 {
		fail(http.StatusForbidden, "missing or invalid token")
		return
	}
	if r.Method != http.MethodPost {
		fail(http.StatusMethodNotAllowed, "use POST")
		return
	}
	s.lastSeen.Store(time.Now().UnixNano())
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		fail(http.StatusBadRequest, "request is too large")
		return
	}
	res, err := s.api.Call(strings.TrimPrefix(r.URL.Path, "/api/"), body)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func isLoopback(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}
