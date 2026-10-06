//go:build windows

package core

// syncDir does nothing on Windows: a directory cannot be opened for FlushFileBuffers there
// (Access is denied), and NTFS journals the rename itself. The archive file was fsynced before it.
func syncDir(string) error { return nil }
