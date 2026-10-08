package agentgo

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

type textArtifact struct {
	id, kind, text string
}

func (a textArtifact) ID() string   { return a.id }
func (a textArtifact) Kind() string { return a.kind }

type numericArtifact struct {
	id    string
	value int
}

func (a numericArtifact) ID() string   { return a.id }
func (a numericArtifact) Kind() string { return "number" }

func TestArtifactManagerIdentityAndReplacement(t *testing.T) {
	manager := newArtifactManager()
	original := textArtifact{"shared", "file", "original"}
	if err := manager.AddArtifact(original, false); err != nil {
		t.Fatal(err)
	}
	replacement := numericArtifact{"shared", 42}
	if err := manager.AddArtifact(replacement, false); !errors.Is(err, ErrArtifactExists) {
		t.Fatalf("duplicate ID with different kind: %v", err)
	}
	if got, _ := manager.GetArtifact("shared"); got != original {
		t.Fatalf("conflict changed value: %v", got)
	}
	if err := manager.AddArtifact(replacement, true); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.GetArtifact("shared"); !ok || got != replacement {
		t.Fatalf("replacement: %v, %v", got, ok)
	}
	// Identity is scoped to the manager, not a global registry.
	if err := newArtifactManager().AddArtifact(original, false); err != nil {
		t.Fatal(err)
	}
	if !manager.DeleteArtifact("shared") || manager.DeleteArtifact("shared") {
		t.Fatal("delete must report whether the ID existed")
	}
	if _, ok := manager.GetArtifact("shared"); ok {
		t.Fatal("deleted artifact still registered")
	}
	if len(manager.ListArtifacts()) != 0 {
		t.Fatal("deleted artifact still listed")
	}
}

func TestArtifactManagerListIsStableAndIndependent(t *testing.T) {
	manager := newArtifactManager()
	for _, id := range []string{"z", "a", "m"} {
		if err := manager.AddArtifact(textArtifact{id, "custom", id}, true); err != nil {
			t.Fatal(err)
		}
	}
	listed := manager.ListArtifacts()
	for i, id := range []string{"a", "m", "z"} {
		if listed[i].ID() != id {
			t.Fatalf("position %d: %q, want %q", i, listed[i].ID(), id)
		}
	}
	listed[0] = nil
	if got, ok := manager.GetArtifact("a"); !ok || got == nil {
		t.Fatal("list aliases manager storage")
	}
}

func TestArtifactManagerConcurrentAccess(t *testing.T) {
	manager := newArtifactManager()
	var inserted atomic.Int32
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			err := manager.AddArtifact(textArtifact{"contended", "custom", "value"}, false)
			switch {
			case err == nil:
				inserted.Add(1)
			case errors.Is(err, ErrArtifactExists):
			default:
				t.Errorf("add: %v", err)
			}
			id := fmt.Sprintf("worker-%d", i)
			if err := manager.AddArtifact(numericArtifact{id, i}, false); err != nil {
				t.Error(err)
			}
			if got, ok := manager.GetArtifact(id); !ok || got.(numericArtifact).value != i {
				t.Errorf("get %q: %v", id, got)
			}
			_ = manager.ListArtifacts()
			if !manager.DeleteArtifact(id) {
				t.Errorf("delete %q failed", id)
			}
		})
	}
	wg.Wait()
	if inserted.Load() != 1 || len(manager.ListArtifacts()) != 1 {
		t.Fatal("concurrent registration lost uniqueness")
	}
}
