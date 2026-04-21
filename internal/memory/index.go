package memory

// IndexPath is the conventional location of the always-injected table of
// contents inside the memory root.
const IndexPath = "INDEX.md"

// ReadIndex returns the index file contents if present, or "" if absent.
func (m *FS) ReadIndex() string {
	if !m.Exists(IndexPath) {
		return ""
	}
	s, err := m.Read(IndexPath)
	if err != nil {
		return ""
	}
	return s
}
