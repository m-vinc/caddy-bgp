package caddybgp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func init() {
	caddy.RegisterModule(adminAPI{})
}

type adminAPI struct{}

type drainResponse struct {
	Drained       bool                 `json:"drained"`
	Waited        string               `json:"waited,omitempty"`
	Announcements []AnnouncementStatus `json:"announcements"`
}

func (adminAPI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "admin.api.bgp", New: func() caddy.Module { return new(adminAPI) }}
}

func (a adminAPI) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{
		{Pattern: "/bgp/peers", Handler: caddy.AdminHandlerFunc(a.peers)},
		{Pattern: "/bgp/announcements", Handler: caddy.AdminHandlerFunc(a.announcements)},
		{Pattern: "/bgp/drain", Handler: caddy.AdminHandlerFunc(a.drain)},
		{Pattern: "/bgp/undrain", Handler: caddy.AdminHandlerFunc(a.undrain)},
	}
}

func adminRuntime(r *http.Request, method string) (*runtime, error) {
	if r.Method != method {
		return nil, caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: fmt.Errorf("method not allowed: %s, use %s", r.Method, method)}
	}

	shared := sharedRuntime()
	if shared == nil {
		return nil, caddy.APIError{HTTPStatus: http.StatusNotFound, Err: errNotRunning}
	}

	return shared, nil
}

func writeJSON(w http.ResponseWriter, value any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(value)
}

func (adminAPI) peers(w http.ResponseWriter, r *http.Request) error {
	shared, err := adminRuntime(r, http.MethodGet)
	if err != nil {
		return err
	}

	return writeJSON(w, shared.peerStatus())
}

func (adminAPI) announcements(w http.ResponseWriter, r *http.Request) error {
	shared, err := adminRuntime(r, http.MethodGet)
	if err != nil {
		return err
	}

	return writeJSON(w, shared.announcementStatus())
}

func (adminAPI) drain(w http.ResponseWriter, r *http.Request) error {
	return changeDrain(w, r, true)
}

func (adminAPI) undrain(w http.ResponseWriter, r *http.Request) error {
	return changeDrain(w, r, false)
}

func changeDrain(w http.ResponseWriter, r *http.Request, drain bool) error {
	shared, err := adminRuntime(r, http.MethodPost)
	if err != nil {
		return err
	}

	delay, err := shared.setDrain(r.URL.Query().Get("announcement"), drain)
	if err != nil {
		return drainError(err)
	}

	response := drainResponse{Drained: drain}
	if wait, _ := strconv.ParseBool(r.URL.Query().Get("wait")); wait && drain {
		select {
		case <-time.After(delay):
			response.Waited = delay.String()
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}

	response.Announcements = shared.announcementStatus()
	return writeJSON(w, response)
}

func drainError(err error) error {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errUnknownAnnouncement), errors.Is(err, errNotRunning):
		status = http.StatusNotFound
	case errors.Is(err, errAllDrained):
		status = http.StatusConflict
	}

	return caddy.APIError{HTTPStatus: status, Err: err}
}

var _ caddy.AdminRouter = (*adminAPI)(nil)
