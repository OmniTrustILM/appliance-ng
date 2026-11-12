package firstboot

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// State is the setup phase the appliance is in.
type State string

// Setup phases in the order a fresh appliance passes through them.
const (
	StateWaitingForAddress State = "waiting-for-address"
	StateConfiguring       State = "configuring"
	StateInstalling        State = "installing"
	StateBootstrapping     State = "bootstrapping"
	StateAddons            State = "addons"
	StatePlatform          State = "platform"
	StateReady             State = "ready"
)

// Status is what GET /status reports. Since is when State or Step last changed.
type Status struct {
	State   State     `json:"state"`
	Step    string    `json:"step"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// StatusStore holds the current status and the node address shown on the console.
type StatusStore struct {
	mu      sync.Mutex
	status  Status
	nodeIP  string
	changed chan struct{}
}

// NewStatusStore returns an empty store.
func NewStatusStore() *StatusStore {
	return &StatusStore{status: Status{Since: time.Now().UTC()}, changed: make(chan struct{}, 1)}
}

// Get returns the current status.
func (s *StatusStore) Get() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// set updates the status and reports whether the state or step changed and whether the message changed.
func (s *StatusStore) set(state State, step, message string) (moved, messageChanged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	moved = s.status.State != state || s.status.Step != step
	messageChanged = s.status.Message != message
	if moved {
		s.status.Since = time.Now().UTC()
	}
	s.status.State, s.status.Step, s.status.Message = state, step, message
	if moved {
		select {
		case s.changed <- struct{}{}:
		default:
		}
	}
	return moved, messageChanged
}

func (s *StatusStore) setNodeIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodeIP = ip
}

func (s *StatusStore) consoleView() (State, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status.State, s.nodeIP
}

// Handler serves GET /status and nothing else. It uses a private mux so that handlers registered on
// http.DefaultServeMux by imported packages are never exposed.
func (s *StatusStore) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Get())
	})
	return mux
}

// NewServer returns the status server for addr with timeouts set.
func NewServer(addr string, s *StatusStore) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}
