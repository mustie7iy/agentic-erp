// Package mergepatch applies JSON Merge Patch (RFC 7396) to decoded JSON
// objects: the PATCH semantics of the API (docs/design/04-api-design.md
// §4.3), used on custom fields (ADR-0009).
package mergepatch

// Apply returns target with patch merged in: a null member removes the key,
// an object member merges recursively (into an empty object when the target
// member is missing or not an object), and any other value replaces. Target
// itself is not modified.
func Apply(target, patch map[string]any) map[string]any {
	out := make(map[string]any, len(target))
	for k, v := range target {
		out[k] = v
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(out, k)
		case map[string]any:
			tv, _ := out[k].(map[string]any) // a missing or non-object member merges as {}
			out[k] = Apply(tv, pv)
		default:
			out[k] = v
		}
	}
	return out
}
