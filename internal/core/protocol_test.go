package core_test

import (
	"errors"
	"testing"

	"github.com/sting8k/piggery/internal/core"
)

// identify exchanges protocol versions: an adapter below the minimum is refused and nothing is
// recorded; one that differs (an adapter from before the exchange sends none: 0) connects, and ps
// and doctor show it until it speaks the daemon's version.
func TestIdentifyProtocolVersion(t *testing.T) {
	f := newFixture(t, nil, core.WithMinProtocolVersion(1))
	var ce *core.Error
	if _, err := f.e.Identify(ctx, f.bob, core.IdentifyArgs{RunID: f.bob.RunID}); !errors.As(err, &ce) ||
		ce.Code != core.CodeUnsupported || ce.RuleID != "protocol_version" {
		t.Fatalf("protocol 0 with minimum 1: %v; want unsupported/protocol_version", err)
	}

	f = newFixture(t, nil) // the default minimum takes an adapter from before the exchange
	res, err := f.e.Identify(ctx, f.bob, core.IdentifyArgs{RunID: f.bob.RunID})
	if err != nil || res.ProtocolVersion != core.ProtocolVersion {
		t.Fatalf("protocol 0: %+v, %v; want the daemon's version back", res, err)
	}
	mismatch := func() (ps *int, findings int) {
		t.Helper()
		st, err := f.e.State(ctx, core.StateArgs{})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range st.Teams[0].Members {
			if m.ID == f.bob.ParticipantID {
				ps = m.ProtocolVersion
			}
		}
		d, err := f.e.Doctor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range d.Findings {
			if x.Kind == "protocol" {
				findings++
			}
		}
		return ps, findings
	}
	if v, n := mismatch(); v == nil || *v != 0 || n != 1 {
		t.Fatalf("after protocol 0: ps %v, %d doctor findings; want 0 and 1", v, n)
	}
	if _, err := f.e.Identify(ctx, f.bob, core.IdentifyArgs{RunID: f.bob.RunID, ProtocolVersion: core.ProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	if v, n := mismatch(); v == nil || *v != core.ProtocolVersion || n != 0 {
		t.Fatalf("after the daemon's version: ps %v, %d doctor findings; want it and none", v, n)
	}
}
