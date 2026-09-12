// Package state records what the agent did, so a human (or a future control plane) can ask what happened without re-deriving it.
//
// This is metadata, never the source of truth.
// If the file is deleted the agent recomputes everything from Git and the host on the next reconcile, the only thing lost is history.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/podcd/podcd/internal/atomicfile"
	"github.com/podcd/podcd/pkg/model"
)

// Version of the on-disk format.
const Version = 1

// Store reads and writes agent metadata.
//
// It is an interface so the JSON file can become SQLite later without anything else changing.
// It is a JSON file today because one small readable file beats a database before the first container runs.
type Store interface {
	Load() (State, error)
	Save(State) error
	Path() string
}

// Attempt records one reconcile attempt.
type Attempt struct {
	At        time.Time         `json:"at"`
	Revisions map[string]string `json:"revisions,omitempty"`
	Actions   []string          `json:"actions,omitempty"`
	Error     string            `json:"error,omitempty"`
	Duration  string            `json:"duration,omitempty"`
}

// AppRecord is what the agent last did to one application.
type AppRecord struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	SpecHash  string `json:"specHash"`
	AppliedAt string `json:"appliedAt,omitempty"`
	Revision  string `json:"revision,omitempty"`

	Health        string `json:"health,omitempty"`
	HealthMessage string `json:"healthMessage,omitempty"`
	HealthAt      string `json:"healthAt,omitempty"`

	// Previous deployment, kept so a human can see what changed and what to roll back to.
	PreviousImage    string `json:"previousImage,omitempty"`
	PreviousSpecHash string `json:"previousSpecHash,omitempty"`
	PreviousRevision string `json:"previousRevision,omitempty"`
}

// State is the whole local metadata document.
type State struct {
	Version   int    `json:"version"`
	Host      string `json:"host,omitempty"`
	MachineID string `json:"machineId,omitempty"`
	AgentID   string `json:"agentId,omitempty"`

	LastAttempt  *Attempt `json:"lastAttempt,omitempty"`
	LastSuccess  *Attempt `json:"lastSuccess,omitempty"`
	LastFailure  *Attempt `json:"lastFailure,omitempty"`
	FailureCount int      `json:"consecutiveFailures"`

	Applications map[string]AppRecord `json:"applications,omitempty"`
}

// RecordApplied updates an application's record after a successful apply, keeping the previous deployment's identifiers.
func (s *State) RecordApplied(app model.Application, revision string, at time.Time) {
	if s.Applications == nil {
		s.Applications = map[string]AppRecord{}
	}
	rec := s.Applications[app.Name]
	hash := app.SpecHash()
	if rec.SpecHash != "" && rec.SpecHash != hash {
		rec.PreviousImage = rec.Image
		rec.PreviousSpecHash = rec.SpecHash
		rec.PreviousRevision = rec.Revision
	}
	rec.Name = app.Name
	rec.Image = app.Image
	rec.SpecHash = hash
	rec.Revision = revision
	rec.AppliedAt = at.UTC().Format(time.RFC3339)
	s.Applications[app.Name] = rec
}

// RecordHealth stores the latest probe result for an application.
func (s *State) RecordHealth(h model.Health) {
	if s.Applications == nil {
		s.Applications = map[string]AppRecord{}
	}
	rec, ok := s.Applications[h.App]
	if !ok {
		rec = AppRecord{Name: h.App}
	}
	rec.Health = string(h.Status)
	rec.HealthMessage = h.Message
	rec.HealthAt = h.CheckedAt.UTC().Format(time.RFC3339)
	s.Applications[h.App] = rec
}

// Forget drops an application that no longer exists.
func (s *State) Forget(app string) { delete(s.Applications, app) }

// FileStore is a Store backed by one JSON file.
type FileStore struct{ path string }

// NewFileStore returns a store writing to path.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Path implements Store.
func (f *FileStore) Path() string { return f.path }

// Load reads the state, returning an empty state when the file does not exist.
func (f *FileStore) Load() (State, error) {
	s := State{Version: Version, Applications: map[string]AppRecord{}}
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, fmt.Errorf("reading state %s: %w", f.path, err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		// Corrupt metadata must not stop the agent from converging the host.
		// Start fresh and say so.
		return State{Version: Version, Applications: map[string]AppRecord{}},
			fmt.Errorf("state file %s is unreadable (%w); starting a new one", f.path, err)
	}
	if s.Applications == nil {
		s.Applications = map[string]AppRecord{}
	}
	s.Version = Version
	return s, nil
}

// Save writes the state atomically, readable only by the agent user.
func (f *FileStore) Save(s State) error {
	s.Version = Version
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicfile.Write(f.path, data, 0o600)
}

var _ Store = (*FileStore)(nil)
