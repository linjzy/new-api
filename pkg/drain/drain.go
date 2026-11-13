// Package drain coordinates local deployment admission and connection shutdown.
package drain

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

var Default = New()

type Status struct {
	Protocol     int    `json:"protocol"`
	JobID        string `json:"job_id"`
	Draining     bool   `json:"draining"`
	HTTPInflight int    `json:"http_inflight"`
	WSBusy       int    `json:"ws_busy"`
	WSIdle       int    `json:"ws_idle"`
	WSClosing    int    `json:"ws_closing"`
	Ready        bool   `json:"ready"`
}

type Manager struct {
	mu       sync.Mutex
	jobID    string
	shutdown bool
	http     int
	sessions map[*Session]struct{}
	changed  chan struct{}
}

type admissionKey struct{}
type admission struct {
	manager *Manager
	active  bool // protected by manager.mu
}

type Session struct {
	manager *Manager
	busy    bool // all session fields are protected by manager.mu
	closing bool
	close   func()
}

func New() *Manager {
	return &Manager{sessions: make(map[*Session]struct{}), changed: make(chan struct{})}
}

func (m *Manager) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Protocol: 1, JobID: m.jobID, Draining: m.jobID != "", HTTPInflight: m.http}
	for session := range m.sessions {
		switch {
		case session.closing:
			s.WSClosing++
		case session.busy:
			s.WSBusy++
		default:
			s.WSIdle++
		}
	}
	s.Ready = s.Draining && m.http == 0 && len(m.sessions) == 0
	return s
}

// Begin is idempotent for the same job. Idle connections close immediately;
// busy connections close only after their request worker finishes its output.
func (m *Manager) Begin(jobID string) error {
	m.mu.Lock()
	if jobID == "" || (m.jobID != "" && m.jobID != jobID) || m.shutdown {
		m.mu.Unlock()
		return errors.New("drain is owned by another job or shutdown")
	}
	callbacks := m.beginLocked(jobID)
	m.mu.Unlock()
	for _, closeSession := range callbacks {
		go closeSession()
	}
	return nil
}

func (m *Manager) beginLocked(jobID string) []func() {
	m.jobID = jobID
	var callbacks []func()
	for session := range m.sessions {
		if !session.busy && !session.closing {
			session.closing = true
			callbacks = append(callbacks, session.close)
		}
	}
	m.notifyLocked()
	return callbacks
}

func (m *Manager) Cancel(jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if jobID == "" || m.jobID != jobID || m.shutdown {
		return errors.New("drain is owned by another job or shutdown")
	}
	m.jobID = ""
	m.notifyLocked()
	return nil
}

func (m *Manager) BeginShutdown() {
	m.mu.Lock()
	// Shutdown takes over deployment admission and cannot subsequently be canceled.
	m.shutdown = true
	callbacks := m.beginLocked("shutdown")
	m.mu.Unlock()
	for _, closeSession := range callbacks {
		go closeSession()
	}
}

func (m *Manager) Wait(ctx context.Context) error {
	for {
		m.mu.Lock()
		ready := m.jobID != "" && m.http == 0 && len(m.sessions) == 0
		changed := m.changed
		m.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Wrap holds admission through the entire handler, including settlement defers.
// Health checks remain available and are excluded from the business counters.
func (m *Manager) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		m.mu.Lock()
		if m.jobID != "" {
			m.mu.Unlock()
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"service restarting; retry later","type":"service_unavailable","code":"service_draining"}}`))
			return
		}
		m.http++
		a := &admission{manager: m, active: true}
		m.notifyLocked()
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			if a.active {
				m.http--
				a.active = false
				m.notifyLocked()
			}
			m.mu.Unlock()
		}()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), admissionKey{}, a)))
	})
}

// RegisterWS atomically transfers the HTTP handshake admission to a session.
// The continuous realtime protocol stays busy for its complete lifetime: it has
// no universally safe request boundary, so deployments wait instead of cutting it.
func RegisterWS(ctx context.Context, busy bool, closeSession func()) *Session {
	m := Default
	a, _ := ctx.Value(admissionKey{}).(*admission)
	if a != nil {
		m = a.manager
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobID != "" {
		return nil
	}
	if a != nil && a.active {
		a.active = false
		m.http--
	}
	s := &Session{manager: m, busy: busy, close: closeSession}
	m.sessions[s] = struct{}{}
	m.notifyLocked()
	return s
}

// StartWork shares the admission lock with Begin, so a successful idle
// observation can never race a new response.create into the upstream.
func (s *Session) StartWork() bool {
	if s == nil {
		return true
	}
	m := s.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[s]; !exists || m.jobID != "" || s.closing || s.busy {
		return false
	}
	s.busy = true
	m.notifyLocked()
	return true
}

func (s *Session) Draining() bool {
	if s == nil {
		return false
	}
	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()
	return s.manager.jobID != "" || s.closing
}

func (s *Session) FinishWork() {
	if s == nil {
		return
	}
	m := s.manager
	m.mu.Lock()
	s.busy = false
	closeNow := m.jobID != "" && !s.closing
	if closeNow {
		s.closing = true
	}
	m.notifyLocked()
	m.mu.Unlock()
	if closeNow {
		s.close()
	}
}

func (s *Session) Unregister() {
	if s == nil {
		return
	}
	m := s.manager
	m.mu.Lock()
	delete(m.sessions, s)
	m.notifyLocked()
	m.mu.Unlock()
}
