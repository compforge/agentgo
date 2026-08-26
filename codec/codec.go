// Package codec encodes typed Go value trees into portable representations.
//
// Struct fields participate only when they carry a codec tag, for example
// `codec:"name,omitempty"`. Concrete values stored behind non-empty interfaces
// must also be registered with a stable TypeID so a later process can
// reconstruct their Go type without relying on package or type names.
package codec

// TypeID is the stable identity written for a registered concrete type.
// It is part of the encoded contract and should not be derived from a Go type
// name that may change during refactoring.
type TypeID string

// Codec converts values to and from an external representation.
type Codec interface {
	Marshal(value any) ([]byte, error)
	Unmarshal(data []byte, target any) error
}

// Option configures a Codec registry.
type Option interface {
	apply(*registry) error
}

// Handler defines a custom representation for T. R follows the same codec-tag
// rules and is encoded recursively, so it may contain tagged interface values.
type Handler[T, R any] interface {
	Encode(T) (R, error)
	Decode(R) (T, error)
}

// TaggedValue is the external envelope used for registered concrete types.
type TaggedValue struct {
	Type  TypeID `json:"$type"`
	Value any    `json:"value"`
}

// Type registers T with the default tag-driven struct codec.
func Type[T any](id TypeID) Option {
	return registrationOption{
		id:             id,
		goType:         typeOf[T](),
		defaultHandler: true,
	}
}

// WithHandler registers T with a custom representation handler.
func WithHandler[T, R any](id TypeID, handler Handler[T, R]) Option {
	return registrationOption{
		id:                 id,
		goType:             typeOf[T](),
		representationType: typeOf[R](),
		encode: func(value any) (any, error) {
			typed, err := coerce[T](value)
			if err != nil {
				return nil, err
			}
			return handler.Encode(typed)
		},
		decode: func(representation any) (any, error) {
			typed, err := coerce[R](representation)
			if err != nil {
				return nil, err
			}
			return handler.Decode(typed)
		},
	}
}
