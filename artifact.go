package agentgo

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Artifact is application-owned material with an identity independent of the
// message transcript. It can hold intermediate/final products or other named
// application data. Applications define concrete payloads and the meaning of
// ID and Kind; AgentGo does not extract, render, or version them.
// ID must remain stable while the artifact is registered.
type Artifact interface {
	ID() string
	Kind() string
}

// ErrArtifactExists means AddArtifact found the ID and overwrite was false.
var ErrArtifactExists = errors.New("artifact already exists")

// ArtifactManager manages material independently of AgentMessage history.
// AgentGo owns its lifetime and provides it in hook, middleware, and transform
// inputs. Use the capability during the callback rather than retaining it.
// Message associations and prompt policy belong to application components.
// Values are shared, not deep-copied: publish replacements or synchronize any
// in-place mutation. IDs must remain stable while registered.
type ArtifactManager interface {
	// AddArtifact registers a non-nil artifact. If its ID already exists,
	// overwrite replaces the entire value, including its concrete type and Kind;
	// otherwise the existing value is preserved and ErrArtifactExists is returned.
	AddArtifact(artifact Artifact, overwrite bool) error
	GetArtifact(id string) (Artifact, bool)
	// ListArtifacts returns a new slice sorted by ID for reproducible traversal.
	ListArtifacts() []Artifact
	// DeleteArtifact reports whether an entry was removed.
	DeleteArtifact(id string) bool
}

// newArtifactManager creates the runtime-owned material collection. Payloads
// are retained as supplied; applications publish replacements or synchronize
// mutations of their own concrete values.
func newArtifactManager() *memoryArtifactManager {
	return &memoryArtifactManager{artifacts: make(map[string]Artifact)}
}

type memoryArtifactManager struct {
	mu        sync.RWMutex
	artifacts map[string]Artifact
	changed   bool
}

func (m *memoryArtifactManager) AddArtifact(artifact Artifact, overwrite bool) error {
	if artifact == nil {
		return fmt.Errorf("artifact must not be nil")
	}
	id := artifact.ID()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.artifacts[id]; exists && !overwrite {
		return fmt.Errorf("%w: %q", ErrArtifactExists, id)
	}
	m.artifacts[id] = artifact
	m.changed = true
	return nil
}

func (m *memoryArtifactManager) GetArtifact(id string) (Artifact, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	artifact, ok := m.artifacts[id]
	return artifact, ok
}

func (m *memoryArtifactManager) ListArtifacts() []Artifact {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.artifacts))
	for id := range m.artifacts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	artifacts := make([]Artifact, 0, len(ids))
	for _, id := range ids {
		artifacts = append(artifacts, m.artifacts[id])
	}
	return artifacts
}

func (m *memoryArtifactManager) DeleteArtifact(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.artifacts[id]
	delete(m.artifacts, id)
	m.changed = m.changed || exists
	return exists
}

// artifactValues validates an entire restore before any live state changes.
func artifactValues(values []Artifact) (map[string]Artifact, error) {
	manager := newArtifactManager()
	for _, value := range values {
		if err := manager.AddArtifact(value, false); err != nil {
			return nil, err
		}
	}
	return manager.artifacts, nil
}

func (m *memoryArtifactManager) replace(values map[string]Artifact) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.artifacts = values
}
