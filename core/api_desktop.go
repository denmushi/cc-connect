package core

import (
	"encoding/json"
	"net/http"
)

type DesktopRequest struct {
	SessionID string `json:"session_id"`
	Action    string `json:"action"`
}

func (s *APIServer) handleDesktop(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	engines := make([]*Engine, 0, len(s.engines))
	for _, e := range s.engines {
		engines = append(engines, e)
	}
	s.mu.RUnlock()
	rows := []DesktopSession{}
	for _, e := range engines {
		rows = append(rows, e.DesktopSessions()...)
	}
	if r.Method == http.MethodGet {
		apiJSON(w, http.StatusOK, rows)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req DesktopRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.SessionID == "" {
		http.Error(w, "session_id and action are required", http.StatusBadRequest)
		return
	}
	var matches []DesktopSession
	for _, row := range rows {
		if row.SessionID == req.SessionID {
			matches = append(matches, row)
		}
	}
	if len(matches) != 1 {
		http.Error(w, "必须指定唯一的当前原生会话 ID", http.StatusConflict)
		return
	}
	for _, e := range engines {
		if e.name != matches[0].Project {
			continue
		}
		if err := e.DesktopSessionAction(r.Context(), matches[0], req.Action); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		apiJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
}
