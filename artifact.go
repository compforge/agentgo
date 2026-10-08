package agentgo

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Artifact is application-owned material with an identity independent of the
// message transcript. Applications define concrete payloads and the meaning of
// ID and Kind; AgentGo does not extract, render, or version them.
// ID must remain stable while the artifact is registered.
type Artifact interface {
	ID() string
	Kind() string
}

// ErrArtifactExists means AddArtifact found the ID and overwrite was false.
var ErrArtifactExists = errors.New("artifact already exists")

// ArtifactManager manages material independently of AgentMessage history.
// Hosts choose its lifetime and inject the same instance into run hooks, tools,
// middleware, and context transformers. Message associations and prompt policy
// belong to those application components.
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

// NewArtifactManager returns an empty, concurrency-safe in-memory manager.
// Values are stored and returned as supplied, without deep copying. Applications
// must synchronize mutations of their payloads, or publish replacement values
// with AddArtifact instead. Manager contents are not part of AgentSnapshot;
// hosts own persistence and reconstruction.
func NewArtifactManager() ArtifactManager {
	return &memoryArtifactManager{artifacts: make(map[string]Artifact)}
}

type memoryArtifactManager struct {
	mu        sync.RWMutex
	artifacts map[string]Artifact
}

func (m *memoryArtifactManager) AddArtifact(artifact Artifact, overwrite bool) error {
	id := artifact.ID()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.artifacts[id]; exists && !overwrite {
		return fmt.Errorf("%w: %q", ErrArtifactExists, id)
	}
	m.artifacts[id] = artifact
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
	return exists
}
