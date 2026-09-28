package core

// SetGCAfterSnapshot sets the gc test seam (between reading a team's rows and deleting them).
func SetGCAfterSnapshot(f func()) func() {
	old := gcAfterSnapshot
	gcAfterSnapshot = f
	return func() { gcAfterSnapshot = old }
}
