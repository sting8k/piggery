// Helpers for tests that stand in for the daemon: where a fake daemon listens, and the home the
// extensions find it from. On unix that is ~/.piggery/piggery.sock; on Windows a named pipe (Node
// accepts nothing else there) whose name goes to ~/.piggery/piggery.pipe, as the daemon writes it.
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

/** Makes dir the home of this process: HOME on unix, USERPROFILE on Windows (os.homedir reads it). */
export function setHome(dir) {
	process.env.HOME = dir;
	process.env.USERPROFILE = dir;
}

/** A path for a fake daemon to listen on; home's ~/.piggery/ names it for the extensions. */
export function fakeDaemonPath(home) {
	mkdirSync(join(home, ".piggery"), { recursive: true });
	if (process.platform !== "win32") return join(home, ".piggery", "piggery.sock");
	const pipe = `\\\\.\\pipe\\piggery-test-${process.pid}-${Date.now()}`;
	writeFileSync(join(home, ".piggery", "piggery.pipe"), pipe + "\n");
	return pipe;
}
