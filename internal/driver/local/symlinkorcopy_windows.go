//go:build windows

package local

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// symlinkOrCopy makes dst stand for src. A symlink needs a privilege most users have not (Developer
// Mode off); without it dst is the next best thing (standIn).
func symlinkOrCopy(src, dst string) error {
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	return standIn(src, dst)
}

// standIn makes dst stand for src with what any user may create: a junction for a directory, a hard
// link for a file. Both are still src itself, as a symlink is: what a worker writes there (its
// sessions, a refreshed login) is written to the human's own directory, and nothing is copied at
// each spawn. Only where neither can be made (a file on another volume, a file system without them)
// is dst a copy of src as it is now.
func standIn(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if junction(src, dst) == nil {
			return nil
		}
	} else if os.Link(src, dst) == nil {
		return nil
	}
	return copyTree(src, dst)
}

// maxReparse is the most a reparse point holds (MAXIMUM_REPARSE_DATA_BUFFER_SIZE).
const maxReparse = 16 << 10

// junction makes dst a junction to the directory src: an empty directory given a mount-point
// reparse point (FSCTL_SET_REPARSE_POINT; a REPARSE_DATA_BUFFER holding the target as an NT path
// and as people read it). Removing dst removes the junction, never what is in src.
func junction(src, dst string) error {
	target, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	target = strings.TrimPrefix(target, `\\?\`)
	sub, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		return err
	}
	shown, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	// The header (tag, data length, reserved), the offset and length in bytes of each name, then
	// the names, each with its NUL (which the lengths leave out).
	const header, names = 8, 8
	subLen, shownLen := 2*(len(sub)-1), 2*(len(shown)-1)
	buf := make([]byte, header+names+subLen+2+shownLen+2)
	if len(buf) > maxReparse {
		return errors.New("junction target too long")
	}
	le := binary.LittleEndian
	le.PutUint32(buf[0:], windows.IO_REPARSE_TAG_MOUNT_POINT)
	le.PutUint16(buf[4:], uint16(len(buf)-header))
	le.PutUint16(buf[8:], 0) // substitute name: at 0
	le.PutUint16(buf[10:], uint16(subLen))
	le.PutUint16(buf[12:], uint16(subLen+2)) // print name: after it and its NUL
	le.PutUint16(buf[14:], uint16(shownLen))
	at := header + names
	for _, u := range append(sub, shown...) {
		le.PutUint16(buf[at:], u)
		at += 2
	}

	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	if err := setReparsePoint(dst, buf); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

func setReparsePoint(dir string, buf []byte) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var n uint32
	return windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &n, nil)
}

// copyTree makes dst a copy of the file or directory src.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o600)
	})
}
