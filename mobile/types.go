package mobile

import (
	"sync"
	"time"
)

// --- Helpers for gomobile type compatibility ---

type FileListResponse struct {
	Remote *StringArray
	Local  *StringArray
}

type StringArray struct {
	Values []string
}

func (a *StringArray) Size() int {
	return len(a.Values)
}

func (a *StringArray) Get(i int) string {
	if i < 0 || i >= len(a.Values) {
		return ""
	}
	return a.Values[i]
}

// Opstates for Swift.

const (
	OpStatusIdle      = "idle"
	OpStatusRunning   = "running"
	OpStatusSucceeded = "succeeded"
	OpStatusFailed    = "failed"
	OpStatusTimeout   = "timeout"
)

// internal operation tracker
type opState struct {
	mu        sync.Mutex
	status    string
	message   string // error message or info
	startedAt time.Time
}

func newOpState() *opState {
	return &opState{status: OpStatusIdle}
}

func (o *opState) set(status, msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status = status
	o.message = msg
	if status == OpStatusRunning {
		o.startedAt = time.Now()
	}
}

func (o *opState) get() (string, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status, o.message
}

type OpStatus struct {
	Status  string
	Message string
}
