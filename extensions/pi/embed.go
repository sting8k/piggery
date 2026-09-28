// Package piext is piggery's pi extension as shipped in the binary:
// `piggery setup pi` unpacks these files into pi's extensions directory. Tests stay out.
package piext

import "embed"

//go:embed index.ts adapter.mjs client.mjs render.mjs package.json tools.json
var Files embed.FS

// Tools is tools.json: the built-in model tools (name, description, JSON Schema parameters),
// defined once for every adapter: the extension registers them from the copy next
// to it, piggery mcp lists them from this one. `{tool:X}` in a text is X's name for the reader.
// A parameter the daemon has to understand is a protocol change: raise core.ProtocolVersion.
var Tools, _ = Files.ReadFile("tools.json")
