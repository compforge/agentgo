package codec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

const (
	fieldTag         = "codec"
	escapedObjectKey = "$object"
)

var (
	anyType           = reflect.TypeOf((*any)(nil)).Elem()
	jsonRawType       = reflect.TypeOf(json.RawMessage{})
	jsonMarshalType   = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	jsonUnmarshalType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
)

type jsonCodec struct {
	registry *registry
}

type escapedObject struct {
	Value map[string]any `json:"$object"`
}

// NewJSON constructs a JSON Codec with an isolated type registry.
func NewJSON(options ...Option) (Codec, error) {
	registered := newRegistry()
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option.apply(registered); err != nil {
			return nil, err
		}
	}
	return &jsonCodec{registry: registered}, nil
}

func (c *jsonCodec) Marshal(value any) ([]byte, error) {
	encoded, err := c.encode(reflect.ValueOf(value), "$", true)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(encoded)
	if err != nil {
		return nil, fmt.Errorf("marshal codec value: %w", err)
	}
	return data, nil
}

func (c *jsonCodec) Unmarshal(data []byte, target any) error {
	if target == nil {
		return fmt.Errorf("unmarshal codec value: target must be a non-nil pointer")
	}
	targetValue := reflect.ValueOf(target)
	if targetValue.Kind() != reflect.Pointer || targetValue.IsNil() {
		return fmt.Errorf("unmarshal codec value: target must be a non-nil pointer, got %T", target)
	}
	decoded, err := c.decode(json.RawMessage(data), targetValue.Elem().Type(), "$")
	if err != nil {
		return err
	}
	if !decoded.IsValid() {
		targetValue.Elem().SetZero()
		return nil
	}
	if !decoded.Type().AssignableTo(targetValue.Elem().Type()) {
		return fmt.Errorf("unmarshal codec value: decoded %s, want %s", decoded.Type(), targetValue.Elem().Type())
	}
	targetValue.Elem().Set(decoded)
	return nil
}

func (c *jsonCodec) encode(value reflect.Value, path string, allowTag bool) (any, error) {
	if !value.IsValid() {
		return nil, nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil, nil
		}
		concrete := value.Elem()
		if value.Type().NumMethod() > 0 && c.registry.writers[concrete.Type()] == nil {
			return nil, fmt.Errorf("encode %s: concrete type %s behind %s is not registered", path, concrete.Type(), value.Type())
		}
		return c.encode(concrete, path, allowTag)
	}
	if nilable(value.Kind()) && value.IsNil() {
		return nil, nil
	}

	if allowTag {
		if registered := c.registry.writers[value.Type()]; registered != nil {
			representation, err := c.encodeRegistered(value, registered, path)
			if err != nil {
				return nil, err
			}
			return TaggedValue{Type: registered.id, Value: representation}, nil
		}
	}

	if value.Kind() == reflect.Pointer {
		return c.encode(value.Elem(), path, allowTag)
	}
	if isJSONLeaf(value) {
		return value.Interface(), nil
	}

	switch value.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		return value.Interface(), nil
	case reflect.Struct:
		return c.encodeStruct(value, path)
	case reflect.Slice, reflect.Array:
		items := make([]any, value.Len())
		for i := 0; i < value.Len(); i++ {
			encoded, err := c.encode(value.Index(i), fmt.Sprintf("%s[%d]", path, i), true)
			if err != nil {
				return nil, err
			}
			items[i] = encoded
		}
		return items, nil
	case reflect.Map:
		return c.encodeMap(value, path)
	default:
		return nil, fmt.Errorf("encode %s: unsupported Go type %s", path, value.Type())
	}
}

func (c *jsonCodec) encodeRegistered(value reflect.Value, registered *registration, path string) (any, error) {
	if registered.defaultHandler {
		return c.encode(normalizeRegisteredValue(value, registered.goType), path, false)
	}
	representation, err := registered.encode(value.Interface())
	if err != nil {
		return nil, fmt.Errorf("encode %s as %q: %w", path, registered.id, err)
	}
	return c.encode(reflect.ValueOf(representation), path, true)
}

func (c *jsonCodec) encodeStruct(value reflect.Value, path string) (any, error) {
	encoded := make(map[string]any)
	seen := make(map[string]struct{})
	taggedFields := 0
	typeInfo := value.Type()
	for i := 0; i < typeInfo.NumField(); i++ {
		fieldInfo := typeInfo.Field(i)
		tag, ok := fieldInfo.Tag.Lookup(fieldTag)
		if !ok || tag == "-" {
			continue
		}
		if fieldInfo.PkgPath != "" {
			return nil, fmt.Errorf("encode %s: unexported field %s has a codec tag", path, fieldInfo.Name)
		}
		name, omitEmpty, err := parseFieldTag(tag)
		if err != nil {
			return nil, fmt.Errorf("encode %s.%s: %w", path, fieldInfo.Name, err)
		}
		taggedFields++
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("encode %s: duplicate codec field name %q", path, name)
		}
		seen[name] = struct{}{}
		fieldValue := value.Field(i)
		if omitEmpty && fieldValue.IsZero() {
			continue
		}
		fieldPath := joinPath(path, name)
		fieldEncoded, err := c.encode(fieldValue, fieldPath, true)
		if err != nil {
			return nil, err
		}
		encoded[name] = fieldEncoded
	}
	if taggedFields == 0 && typeInfo.NumField() > 0 {
		return nil, fmt.Errorf("encode %s: struct %s has no codec-tagged fields; add tags or register a custom handler", path, typeInfo)
	}
	return escapeObject(encoded), nil
}

func (c *jsonCodec) encodeMap(value reflect.Value, path string) (any, error) {
	if value.Type().Key().Kind() != reflect.String {
		return nil, fmt.Errorf("encode %s: map key type %s is not supported", path, value.Type().Key())
	}
	fields := make(map[string]any, value.Len())
	iterator := value.MapRange()
	for iterator.Next() {
		name := iterator.Key().String()
		encoded, err := c.encode(iterator.Value(), joinPath(path, name), true)
		if err != nil {
			return nil, err
		}
		fields[name] = encoded
	}
	return escapeObject(fields), nil
}

func (c *jsonCodec) decode(raw json.RawMessage, target reflect.Type, path string) (reflect.Value, error) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return reflect.Zero(target), nil
	}
	if isJSONDecodeLeaf(target) {
		return c.decodePlain(raw, target, path)
	}

	representation, escaped, err := parseEscapedObject(raw)
	if err != nil {
		return reflect.Value{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if escaped {
		return c.decodeEscapedObject(representation, target, path)
	}

	id, representation, tagged, err := parseTagged(raw)
	if err != nil {
		return reflect.Value{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if tagged {
		registered := c.registry.readers[id]
		if registered == nil {
			return reflect.Value{}, fmt.Errorf("decode %s: codec type %q is not registered", path, id)
		}
		decoded, err := c.decodeRegistered(representation, registered, path)
		if err != nil {
			return reflect.Value{}, err
		}
		if target.Kind() == reflect.Interface {
			if !decoded.Type().Implements(target) {
				return reflect.Value{}, fmt.Errorf("decode %s: registered type %s does not implement %s", path, decoded.Type(), target)
			}
			return decoded, nil
		}
		if decoded.Type().AssignableTo(target) {
			return decoded, nil
		}
		return reflect.Value{}, fmt.Errorf("decode %s: registered type %s cannot be assigned to %s", path, decoded.Type(), target)
	}

	if target.Kind() == reflect.Interface {
		if target.NumMethod() != 0 {
			return reflect.Value{}, fmt.Errorf("decode %s: interface %s requires a registered tagged value", path, target)
		}
		value, err := c.decodeAny(raw, path)
		if err != nil {
			return reflect.Value{}, err
		}
		if value == nil {
			return reflect.Zero(target), nil
		}
		return reflect.ValueOf(value), nil
	}
	return c.decodePlain(raw, target, path)
}

func (c *jsonCodec) decodeRegistered(raw json.RawMessage, registered *registration, path string) (reflect.Value, error) {
	if registered.defaultHandler {
		return c.decode(raw, registered.goType, path+".value")
	}
	representation, err := c.decode(raw, registered.representationType, path+".value")
	if err != nil {
		return reflect.Value{}, err
	}
	decoded, err := registered.decode(representation.Interface())
	if err != nil {
		return reflect.Value{}, fmt.Errorf("decode %s as %q: %w", path, registered.id, err)
	}
	if decoded == nil {
		return reflect.Zero(registered.goType), nil
	}
	value := reflect.ValueOf(decoded)
	if !value.Type().AssignableTo(registered.goType) {
		return reflect.Value{}, fmt.Errorf("decode %s as %q: handler returned %s, want %s", path, registered.id, value.Type(), registered.goType)
	}
	return value, nil
}

func (c *jsonCodec) decodePlain(raw json.RawMessage, target reflect.Type, path string) (reflect.Value, error) {
	if isJSONDecodeLeaf(target) {
		value := reflect.New(target)
		if err := json.Unmarshal(raw, value.Interface()); err != nil {
			return reflect.Value{}, fmt.Errorf("decode %s as %s: %w", path, target, err)
		}
		return value.Elem(), nil
	}
	if target.Kind() == reflect.Pointer {
		decoded, err := c.decode(raw, target.Elem(), path)
		if err != nil {
			return reflect.Value{}, err
		}
		pointer := reflect.New(target.Elem())
		pointer.Elem().Set(decoded)
		return pointer, nil
	}
	switch target.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		value := reflect.New(target)
		if err := json.Unmarshal(raw, value.Interface()); err != nil {
			return reflect.Value{}, fmt.Errorf("decode %s as %s: %w", path, target, err)
		}
		return value.Elem(), nil
	case reflect.Struct:
		return c.decodeStruct(raw, target, path)
	case reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return reflect.Value{}, fmt.Errorf("decode %s as %s: %w", path, target, err)
		}
		result := reflect.MakeSlice(target, len(items), len(items))
		for i, item := range items {
			decoded, err := c.decode(item, target.Elem(), fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(decoded)
		}
		return result, nil
	case reflect.Array:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return reflect.Value{}, fmt.Errorf("decode %s as %s: %w", path, target, err)
		}
		if len(items) != target.Len() {
			return reflect.Value{}, fmt.Errorf("decode %s as %s: got %d items", path, target, len(items))
		}
		result := reflect.New(target).Elem()
		for i, item := range items {
			decoded, err := c.decode(item, target.Elem(), fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(decoded)
		}
		return result, nil
	case reflect.Map:
		return c.decodeMap(raw, target, path)
	default:
		return reflect.Value{}, fmt.Errorf("decode %s: unsupported Go type %s", path, target)
	}
}

func (c *jsonCodec) decodeEscapedObject(raw json.RawMessage, target reflect.Type, path string) (reflect.Value, error) {
	if target.Kind() == reflect.Pointer {
		decoded, err := c.decodeEscapedObject(raw, target.Elem(), path)
		if err != nil {
			return reflect.Value{}, err
		}
		pointer := reflect.New(target.Elem())
		pointer.Elem().Set(decoded)
		return pointer, nil
	}
	switch target.Kind() {
	case reflect.Interface:
		if target.NumMethod() != 0 {
			return reflect.Value{}, fmt.Errorf("decode %s: escaped object cannot implement %s", path, target)
		}
		value, err := c.decodeAnyObject(raw, path)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(value), nil
	case reflect.Struct:
		return c.decodeStruct(raw, target, path)
	case reflect.Map:
		return c.decodeMap(raw, target, path)
	default:
		return reflect.Value{}, fmt.Errorf("decode %s: escaped object cannot be assigned to %s", path, target)
	}
}

func (c *jsonCodec) decodeMap(raw json.RawMessage, target reflect.Type, path string) (reflect.Value, error) {
	if target.Key().Kind() != reflect.String {
		return reflect.Value{}, fmt.Errorf("decode %s: map key type %s is not supported", path, target.Key())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return reflect.Value{}, fmt.Errorf("decode %s as %s: %w", path, target, err)
	}
	result := reflect.MakeMapWithSize(target, len(fields))
	for name, field := range fields {
		decoded, err := c.decode(field, target.Elem(), joinPath(path, name))
		if err != nil {
			return reflect.Value{}, err
		}
		key := reflect.New(target.Key()).Elem()
		key.SetString(name)
		result.SetMapIndex(key, decoded)
	}
	return result, nil
}

func (c *jsonCodec) decodeStruct(raw json.RawMessage, target reflect.Type, path string) (reflect.Value, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return reflect.Value{}, fmt.Errorf("decode %s as %s: %w", path, target, err)
	}
	result := reflect.New(target).Elem()
	seen := make(map[string]struct{})
	taggedFields := 0
	for i := 0; i < target.NumField(); i++ {
		fieldInfo := target.Field(i)
		tag, ok := fieldInfo.Tag.Lookup(fieldTag)
		if !ok || tag == "-" {
			continue
		}
		if fieldInfo.PkgPath != "" {
			return reflect.Value{}, fmt.Errorf("decode %s: unexported field %s has a codec tag", path, fieldInfo.Name)
		}
		name, _, err := parseFieldTag(tag)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("decode %s.%s: %w", path, fieldInfo.Name, err)
		}
		taggedFields++
		if _, duplicate := seen[name]; duplicate {
			return reflect.Value{}, fmt.Errorf("decode %s: duplicate codec field name %q", path, name)
		}
		seen[name] = struct{}{}
		fieldRaw, present := fields[name]
		if !present {
			continue
		}
		decoded, err := c.decode(fieldRaw, fieldInfo.Type, joinPath(path, name))
		if err != nil {
			return reflect.Value{}, err
		}
		result.Field(i).Set(decoded)
	}
	if taggedFields == 0 && target.NumField() > 0 {
		return reflect.Value{}, fmt.Errorf("decode %s: struct %s has no codec-tagged fields; add tags or register a custom handler", path, target)
	}
	return result, nil
}

func (c *jsonCodec) decodeAny(raw json.RawMessage, path string) (any, error) {
	representation, escaped, err := parseEscapedObject(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if escaped {
		return c.decodeAnyObject(representation, path)
	}
	_, _, tagged, err := parseTagged(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if tagged {
		decoded, err := c.decode(raw, anyType, path)
		if err != nil {
			return nil, err
		}
		return decoded.Interface(), nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("decode %s: empty JSON value", path)
	}
	switch raw[0] {
	case '{':
		return c.decodeAnyObject(raw, path)
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		result := make([]any, len(items))
		for i, item := range items {
			value, err := c.decodeAny(item, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			result[i] = value
		}
		return result, nil
	default:
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		return value, nil
	}
}

func (c *jsonCodec) decodeAnyObject(raw json.RawMessage, path string) (map[string]any, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	result := make(map[string]any, len(fields))
	for name, field := range fields {
		value, err := c.decodeAny(field, joinPath(path, name))
		if err != nil {
			return nil, err
		}
		result[name] = value
	}
	return result, nil
}

func parseEscapedObject(raw json.RawMessage) (json.RawMessage, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false, err
	}
	if len(fields) != 1 {
		return nil, false, nil
	}
	value, escaped := fields[escapedObjectKey]
	return value, escaped, nil
}

func parseTagged(raw json.RawMessage) (TypeID, json.RawMessage, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return "", nil, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", nil, false, err
	}
	typeRaw, hasType := fields["$type"]
	valueRaw, hasValue := fields["value"]
	if !hasType || !hasValue {
		return "", nil, false, nil
	}
	var id TypeID
	if err := json.Unmarshal(typeRaw, &id); err != nil {
		return "", nil, false, fmt.Errorf("invalid codec type: %w", err)
	}
	if id == "" {
		return "", nil, false, fmt.Errorf("codec type must not be empty")
	}
	return id, valueRaw, true, nil
}

// escapeObject protects ordinary JSON objects that would otherwise be
// mistaken for codec envelopes. Decoding removes exactly one wrapper and then
// treats the enclosed value as a plain object, preserving arbitrary map keys.
func escapeObject(fields map[string]any) any {
	_, hasType := fields["$type"]
	_, hasValue := fields["value"]
	_, hasEscape := fields[escapedObjectKey]
	if (hasType && hasValue) || (hasEscape && len(fields) == 1) {
		return escapedObject{Value: fields}
	}
	return fields
}

func parseFieldTag(tag string) (name string, omitEmpty bool, err error) {
	parts := strings.Split(tag, ",")
	if parts[0] == "" {
		return "", false, fmt.Errorf("codec field name must not be empty")
	}
	name = parts[0]
	for _, option := range parts[1:] {
		switch option {
		case "omitempty":
			omitEmpty = true
		case "":
		default:
			return "", false, fmt.Errorf("unsupported codec field option %q", option)
		}
	}
	return name, omitEmpty, nil
}

func normalizeRegisteredValue(value reflect.Value, target reflect.Type) reflect.Value {
	if value.Type() == target {
		return value
	}
	if value.Kind() == reflect.Pointer && value.Type().Elem() == target {
		return value.Elem()
	}
	return value
}

func isJSONLeaf(value reflect.Value) bool {
	if value.Type() == jsonRawType || isByteSlice(value.Type()) {
		return true
	}
	if value.CanInterface() && value.Type().Implements(jsonMarshalType) {
		return true
	}
	return value.CanAddr() && value.Addr().Type().Implements(jsonMarshalType)
}

func isByteSlice(value reflect.Type) bool {
	return value.Kind() == reflect.Slice && value.Elem().Kind() == reflect.Uint8
}

func isJSONDecodeLeaf(target reflect.Type) bool {
	if target == jsonRawType || isByteSlice(target) || target.Implements(jsonUnmarshalType) {
		return true
	}
	return target.Kind() != reflect.Pointer && reflect.PointerTo(target).Implements(jsonUnmarshalType)
}

func joinPath(parent, child string) string {
	if parent == "$" {
		return parent + "." + child
	}
	return parent + "." + child
}
