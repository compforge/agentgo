package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/internal/streaming"
)

// +case=`Opaque signed reasoning survives persistence and replay but is filtered by litellm on another endpoint`
func TestProviderStateReplay(t *testing.T) {
	bodies := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
		fmt.Fprint(w, `{"type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"reason","signature":"signed-state"},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
	}))
	defer server.Close()
	makeModel := func(name string) *LiteLLMAdapter {
		m, err := NewModel("anthropic", "m", WithAPIKey("fake"), WithBaseURL(server.URL), WithProviderExtra(map[string]any{"name": name}))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	primary, backup := makeModel("primary"), makeModel("backup")
	result, err := primary.Generate(t.Context(), []agentgo.Message{agentgo.UserMsg("hi")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-bodies
	encoded, err := json.Marshal(result.Message)
	if err != nil {
		t.Fatal(err)
	}
	var restored agentgo.Message
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	state := restored.Content[0].ProviderState
	if state == nil || state.Provider != "primary" || state.Model != "m" {
		t.Fatalf("lost state: %+v", state)
	}
	snapshot := streaming.Snapshot(restored)
	snapshot.Content[0].ProviderState.Data[0] = '!'
	if state.Data[0] != '{' {
		t.Fatal("published state aliases history")
	}
	for _, model := range []*LiteLLMAdapter{primary, backup} {
		_, err := model.Generate(t.Context(), []agentgo.Message{agentgo.UserMsg("hi"), restored, agentgo.UserMsg("continue")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		body := <-bodies
		if strings.Contains(body, "signed-state") != (model == primary) {
			t.Fatalf("replay escaped endpoint scope: %s", body)
		}
	}
}
