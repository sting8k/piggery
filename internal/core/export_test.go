package core

// SetGCAfterSnapshot sets the gc test seam (between reading a team's rows and deleting them).
func SetGCAfterSnapshot(f func()) func() {
	old := gcAfterSnapshot
	gcAfterSnapshot = f
	return func() { gcAfterSnapshot = old }
}

// NameWord is the word a session named from ref starts at (names.go).
func NameWord(ref string) string { return nameNouns[wordStart(ref)] }

// NotifyAfterCommit is the call every mail path ends in (the wake trigger of wake.go).
func (e *Engine) NotifyAfterCommit(id string) { e.notifyAfterCommit(id) }
