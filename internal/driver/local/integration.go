package local

import "strings"

// Integration versions: one integer for each thing piggery installs for a harness. Bump an
// integer when, and only when, what is installed changes (an extension or plugin file, the list
// of hooks, an entry in the harness's config); a rebuild that leaves the installed part as it
// was keeps it. Status compares these integers, never the build version, and what is installed
// carries its integer as PIGGERY_INTEGRATION_VERSION=N.
var integrationVersions = map[string]int{
	"pi":       11,
	"omp":      11,
	"dsh":      12,
	"opencode": 8,
	"claude":   1,
	"codex":    1,
	"paseo":    3,
}

// IntegrationMarker precedes the integer in what is installed.
const IntegrationMarker = "PIGGERY_INTEGRATION_VERSION="

// IntegrationVersion is the integer this binary installs for the named integration (0: unknown).
func IntegrationVersion(name string) int { return integrationVersions[name] }

// ParseIntegration reads the integer that follows IntegrationMarker at the start of s; 0 when s
// does not start with it (an install from before the integers, or none).
func ParseIntegration(s string) int {
	s, ok := strings.CutPrefix(strings.TrimSpace(s), IntegrationMarker)
	if !ok {
		return 0
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
