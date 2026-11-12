package testutil

import (
	"errors"
	"strings"
	"testing"
)

// SPEC-002 11a (fold-in): a gate must not pass with every integration test
// skipped. Locally, `make test-cold` sets STRATA_REQUIRE_DOCKER=1, which turns the skip into a failure; `make test`
// (the inner loop) doesn't, so it still skips.
func TestDockerVerdict(t *testing.T) {
	down := errors.New("cannot connect to the Docker daemon")
	for _, tc := range []struct {
		name     string
		err      error
		env      map[string]string
		wantFail bool
		wantMsg  string // substring; "" means no message at all
	}{
		{"docker up", nil, map[string]string{"CI": "true", "STRATA_REQUIRE_DOCKER": "1"}, false, ""},
		{"inner loop skips", down, nil, false, "skipping"},
		{"CI fails", down, map[string]string{"CI": "true"}, true, "required in CI"},
		{"gate fails", down, map[string]string{"STRATA_REQUIRE_DOCKER": "1"}, true, "STRATA_REQUIRE_DOCKER=1"},
		{"only 1 turns it on", down, map[string]string{"STRATA_REQUIRE_DOCKER": "true"}, false, "skipping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fail, msg := dockerVerdict(tc.err, func(k string) string { return tc.env[k] })
			if fail != tc.wantFail {
				t.Errorf("fail = %v, want %v (msg %q)", fail, tc.wantFail, msg)
			}
			if tc.wantMsg == "" && msg != "" || !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("msg = %q, want it to contain %q", msg, tc.wantMsg)
			}
		})
	}
}
