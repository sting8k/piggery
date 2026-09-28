// Package paseoext is piggery's Paseo plugin as shipped in the binary: `piggery setup paseo`
// writes these files to ~/.piggery/paseo and installs that directory in Paseo, which bundles the
// TypeScript itself (no build step, no runtime dependencies). Tests, package.json, the lockfile
// and tsconfig.json stay out; client/, server/ and shared/ hold files only, no subdirectories.
package paseoext

import "embed"

//go:embed paseo-plugin.json index.client.tsx index.server.ts client/* server/* shared/*
var Files embed.FS
