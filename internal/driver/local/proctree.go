package local

// A harness runs its tools in child processes, often each in a process group of its own (pi's
// bash tool does), so signalling the worker's group misses them; and once the harness dies its
// children reparent to init and can no longer be found. So the tree is read first, from the
// worker's pid down, and every member is signalled: its group when the group's leader is in the
// tree too, otherwise the pid alone. The OS pieces (processTable, signalTree, the kill and group
// calls the runner makes) are in proctree_unix.go and proctree_windows.go.

// proc is one row of the process table. start is the OS's start identity of the process, as a
// string (unix: `ps lstart` verbatim; windows: the creation time): a pid is the same process only
// while its start is too.
type proc struct {
	pid, ppid, pgid int
	start           string
}

// treeBelow is the descendants of root (root itself not included; it is signalled by its group),
// plus every process of known that is still the same one, with their descendants. known is an
// earlier treeBelow: it finds what has been reparented since. root is a seed only when it is alive
// (a reaped pid may belong to someone else now).
func treeBelow(root int, rootAlive bool, known []proc) []proc {
	rows := processTable()
	byPid := make(map[int]proc, len(rows))
	kids := map[int][]proc{}
	for _, r := range rows {
		byPid[r.pid] = r
		kids[r.ppid] = append(kids[r.ppid], r)
	}
	var out []proc
	seen := map[int]bool{root: true}
	var walk func(pid int)
	walk = func(pid int) {
		for _, k := range kids[pid] {
			if !seen[k.pid] {
				seen[k.pid] = true
				out = append(out, k)
				walk(k.pid)
			}
		}
	}
	if rootAlive {
		walk(root)
	}
	for _, k := range known {
		if r, ok := byPid[k.pid]; ok && k.start != "" && r.start == k.start && !seen[k.pid] {
			seen[k.pid] = true
			out = append(out, r)
			walk(k.pid)
		}
	}
	return out
}
