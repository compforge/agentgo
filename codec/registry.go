package codec

import (
	"fmt"
	"reflect"
)

type registration struct {
	id                 TypeID
	goType             reflect.Type
	representationType reflect.Type
	defaultHandler     bool
	encode             func(any) (any, error)
	decode             func(any) (any, error)
}

type registry struct {
	writers map[reflect.Type]*registration
	readers map[TypeID]*registration
}

func newRegistry() *registry {
	return &registry{
		writers: make(map[reflect.Type]*registration),
		readers: make(map[TypeID]*registration),
	}
}

type registrationOption registration

func (option registrationOption) apply(target *registry) error {
	entry := registration(option)
	if entry.id == "" {
		return fmt.Errorf("codec type ID must not be empty")
	}
	if entry.goType.Kind() == reflect.Interface {
		return fmt.Errorf("codec type %q must be concrete, got %s", entry.id, entry.goType)
	}
	if previous, ok := target.readers[entry.id]; ok {
		return fmt.Errorf("codec type ID %q already registered for %s", entry.id, previous.goType)
	}

	aliases := []reflect.Type{entry.goType}
	if entry.goType.Kind() != reflect.Pointer {
		aliases = append(aliases, reflect.PointerTo(entry.goType))
	}
	for _, alias := range aliases {
		if previous, ok := target.writers[alias]; ok {
			return fmt.Errorf("codec Go type %s already registered as %q", alias, previous.id)
		}
	}

	stored := &entry
	target.readers[entry.id] = stored
	for _, alias := range aliases {
		target.writers[alias] = stored
	}
	return nil
}

func typeOf[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

func coerce[T any](value any) (T, error) {
	var zero T
	target := typeOf[T]()
	if value == nil {
		if nilable(target.Kind()) {
			return zero, nil
		}
		return zero, fmt.Errorf("codec value is nil, want %s", target)
	}
	actual := reflect.ValueOf(value)
	if actual.Type().AssignableTo(target) {
		return actual.Interface().(T), nil
	}
	if actual.Kind() == reflect.Pointer && actual.Type().Elem() == target && !actual.IsNil() {
		return actual.Elem().Interface().(T), nil
	}
	return zero, fmt.Errorf("codec value has type %s, want %s", actual.Type(), target)
}

func nilable(kind reflect.Kind) bool {
	switch kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return true
	default:
		return false
	}
}
