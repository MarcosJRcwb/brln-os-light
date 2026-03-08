package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleNodeRetirementStatusGet(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		payload := map[string]any{
			"available": false,
		}
		if errMsg != "" {
			payload["error"] = errMsg
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	status, err := svc.Status(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"error":     err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleNodeRetirementSessionsPost(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		if errMsg == "" {
			errMsg = "node retirement unavailable"
		}
		writeError(w, http.StatusServiceUnavailable, errMsg)
		return
	}

	var req struct {
		Source             string `json:"source"`
		DryRun             bool   `json:"dry_run"`
		DisclaimerAccepted bool   `json:"disclaimer_accepted"`
		Config             any    `json:"config"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	configRaw := []byte(`{}`)
	if req.Config != nil {
		raw, err := json.Marshal(req.Config)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid config")
			return
		}
		configRaw = raw
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	session, err := svc.CreateSession(ctx, NodeRetirementCreateParams{
		Source:             req.Source,
		DryRun:             req.DryRun,
		DisclaimerAccepted: req.DisclaimerAccepted,
		Config:             configRaw,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrNodeRetirementInvalidSource):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNodeRetirementActiveSession):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) handleNodeRetirementSessionsGet(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		if errMsg == "" {
			errMsg = "node retirement unavailable"
		}
		writeError(w, http.StatusServiceUnavailable, errMsg)
		return
	}

	limit := nodeRetirementDefaultListLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	items, err := svc.ListSessions(ctx, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleNodeRetirementSessionGet(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		if errMsg == "" {
			errMsg = "node retirement unavailable"
		}
		writeError(w, http.StatusServiceUnavailable, errMsg)
		return
	}

	sessionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	session, err := svc.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrNodeRetirementSessionNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleNodeRetirementSessionEventsGet(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		if errMsg == "" {
			errMsg = "node retirement unavailable"
		}
		writeError(w, http.StatusServiceUnavailable, errMsg)
		return
	}

	sessionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}

	limit := nodeRetirementDefaultEventLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	events, err := svc.ListSessionEvents(ctx, sessionID, limit)
	if err != nil {
		if errors.Is(err, ErrNodeRetirementSessionNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": events})
}

func (s *Server) handleNodeRetirementSessionChannelsGet(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		if errMsg == "" {
			errMsg = "node retirement unavailable"
		}
		writeError(w, http.StatusServiceUnavailable, errMsg)
		return
	}

	sessionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	channels, err := svc.ListSessionChannels(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrNodeRetirementSessionNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": channels})
}

func (s *Server) handleNodeRetirementSessionConfirmCoopPost(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		if errMsg == "" {
			errMsg = "node retirement unavailable"
		}
		writeError(w, http.StatusServiceUnavailable, errMsg)
		return
	}

	sessionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	session, err := svc.ConfirmCoopClose(ctx, sessionID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNodeRetirementSessionNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrNodeRetirementInvalidState):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleNodeRetirementSessionDecisionPost(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		if errMsg == "" {
			errMsg = "node retirement unavailable"
		}
		writeError(w, http.StatusServiceUnavailable, errMsg)
		return
	}

	sessionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}

	var req struct {
		ChannelPoint string `json:"channel_point"`
		Decision     string `json:"decision"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := svc.SetChannelDecision(ctx, sessionID, req.ChannelPoint, req.Decision); err != nil {
		switch {
		case errors.Is(err, ErrNodeRetirementSessionNotFound), errors.Is(err, ErrNodeRetirementChannelNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrNodeRetirementInvalidDecision):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNodeRetirementInvalidState):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleNodeRetirementSessionTransferGet(w http.ResponseWriter, r *http.Request) {
	svc, errMsg := s.nodeRetirementService()
	if svc == nil {
		if errMsg == "" {
			errMsg = "node retirement unavailable"
		}
		writeError(w, http.StatusServiceUnavailable, errMsg)
		return
	}

	sessionID := strings.TrimSpace(chi.URLParam(r, "id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	transfer, err := svc.GetSessionTransfer(ctx, sessionID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNodeRetirementSessionNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrNodeRetirementTransferNotFound):
			writeJSON(w, http.StatusOK, map[string]any{"item": nil})
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": transfer})
}
