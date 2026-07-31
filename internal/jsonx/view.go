package jsonx

// CanonicalObjectView exposes a validated, read-only JSON object without
// materializing map[string]any. Fields must be returned in lexical key order so
// deterministic walkers can consume the view exactly like a sorted map.
type CanonicalObjectView interface {
	CanonicalJSONFieldCount() (int, bool)
	CanonicalJSONField(index int) (key string, value any)
}

// CanonicalArrayView exposes a validated, read-only JSON array without boxing
// every element into []any.
type CanonicalArrayView interface {
	CanonicalJSONItemCount() (int, bool)
	CanonicalJSONItem(index int) any
}
