package codec

import (
	"reflect"
	"strings"
	"testing"
)

type testValue interface {
	Name() string
}

type taggedTestValue struct {
	Label   string `codec:"label"`
	Payload []byte `codec:"payload"`
	Ignored string
}

func (v *taggedTestValue) Name() string { return v.Label }

type testEnvelope struct {
	Values []testValue `codec:"values"`
}

func TestJSONRoundTripsRegisteredInterfaceValue(t *testing.T) {
	c, err := NewJSON(Type[*taggedTestValue]("test.tagged-value.v1"))
	if err != nil {
		t.Fatal(err)
	}

	data, err := c.Marshal(testEnvelope{Values: []testValue{&taggedTestValue{Label: "kept", Payload: []byte("bytes"), Ignored: "runtime"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "runtime") || !strings.Contains(string(data), `"$type":"test.tagged-value.v1"`) {
		t.Fatalf("encoded value = %s", data)
	}

	var restored testEnvelope
	if err := c.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Values) != 1 {
		t.Fatalf("restored values = %#v", restored.Values)
	}
	value, ok := restored.Values[0].(*taggedTestValue)
	if !ok || value.Label != "kept" || string(value.Payload) != "bytes" || value.Ignored != "" {
		t.Fatalf("restored value = %#v", restored.Values[0])
	}
}

type customValue struct {
	Name string
}

type customRepresentation struct {
	Value string `codec:"value"`
}

type customValueHandler struct{}

func (customValueHandler) Encode(value customValue) (customRepresentation, error) {
	return customRepresentation{Value: strings.ToUpper(value.Name)}, nil
}

func (customValueHandler) Decode(representation customRepresentation) (customValue, error) {
	return customValue{Name: strings.ToLower(representation.Value)}, nil
}

func TestJSONUsesCustomHandler(t *testing.T) {
	c, err := NewJSON(WithHandler[customValue, customRepresentation](
		"test.custom-value.v1",
		customValueHandler{},
	))
	if err != nil {
		t.Fatal(err)
	}

	data, err := c.Marshal(customValue{Name: "AgentGo"})
	if err != nil {
		t.Fatal(err)
	}
	var restored customValue
	if err := c.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Name != "agentgo" {
		t.Fatalf("restored value = %#v", restored)
	}
}

func TestJSONRejectsUnknownTaggedType(t *testing.T) {
	c, err := NewJSON()
	if err != nil {
		t.Fatal(err)
	}
	var target any
	err = c.Unmarshal([]byte(`{"$type":"missing.v1","value":{}}`), &target)
	if err == nil || !strings.Contains(err.Error(), `codec type "missing.v1" is not registered`) {
		t.Fatalf("error = %v", err)
	}
}

func TestJSONRoundTripsEnvelopeShapedObjects(t *testing.T) {
	c, err := NewJSON(Type[*taggedTestValue]("test.tagged-value.v1"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		value map[string]any
	}{
		{
			name: "registered type lookalike",
			value: map[string]any{
				"$type": "test.tagged-value.v1",
				"value": "business payload",
			},
		},
		{
			name: "unknown type lookalike",
			value: map[string]any{
				"$type":  "business.event.v1",
				"value":  "created",
				"source": "metadata",
			},
		},
		{
			name: "escape lookalike",
			value: map[string]any{
				"$object": map[string]any{"nested": true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := map[string]any{"metadata": tt.value}
			data, err := c.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"$object"`) {
				t.Fatalf("encoded object was not escaped: %s", data)
			}

			var restored map[string]any
			if err := c.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(restored, original) {
				t.Fatalf("restored = %#v, want %#v", restored, original)
			}
		})
	}
}

func TestJSONRejectsStructWithoutTags(t *testing.T) {
	type untagged struct{ Value string }
	c, err := NewJSON(Type[untagged]("test.untagged.v1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Marshal(untagged{Value: "lost"}); err == nil || !strings.Contains(err.Error(), "has no codec-tagged fields") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewJSONRejectsDuplicateTypeID(t *testing.T) {
	_, err := NewJSON(
		Type[taggedTestValue]("duplicate.v1"),
		Type[customValue]("duplicate.v1"),
	)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewJSONRejectsDuplicateGoType(t *testing.T) {
	_, err := NewJSON(
		Type[taggedTestValue]("tagged-value.v1"),
		Type[taggedTestValue]("renamed-tagged-value.v1"),
	)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("error = %v", err)
	}
}
