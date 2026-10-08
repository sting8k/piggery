//go:build windows

package cli

import (
	"unsafe"

	"github.com/sting8k/piggery/internal/driver/local"
	"golang.org/x/sys/windows"
)

// Windows has no ps: the parent comes from the process table and the name from the image the
// process runs (claude.exe is claude), the command line from the process itself.

// parentAndName is pid's parent and its image's base name without .exe; ok false when pid is not
// there or cannot be read.
func parentAndName(pid int) (ppid int, name string, ok bool) {
	name = local.ProcessImage(pid)
	if name == "" {
		return 0, "", false
	}
	return local.ParentPID(pid), name, true
}

// processCommandLineInformation is NtQueryInformationProcess's class for the command line (Windows
// 8.1 and later; PROCESS_QUERY_LIMITED_INFORMATION is enough).
const processCommandLineInformation = 60

// processArgs is pid's command line split as CommandLineToArgvW does; nil when it cannot be read.
func processArgs(pid int) []string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)
	var n uint32
	windows.NtQueryInformationProcess(h, processCommandLineInformation, nil, 0, &n) // asks the size
	if n == 0 {
		return nil
	}
	buf := make([]byte, n)
	if err := windows.NtQueryInformationProcess(h, processCommandLineInformation, unsafe.Pointer(&buf[0]), n, &n); err != nil {
		return nil
	}
	args, err := windows.DecomposeCommandLine((*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String())
	if err != nil {
		return nil
	}
	return args
}
