package processmining

// SetMapChunk makes a map read n cases at a time, so that a test of a
// few cases reads several chunks; the returned function restores it.
func SetMapChunk(n int) func() {
	before := mapChunk
	mapChunk = n
	return func() { mapChunk = before }
}
