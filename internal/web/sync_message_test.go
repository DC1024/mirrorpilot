package web

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/ghsync"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// TestEveryGithubSentinelHasItsOwnMessage walks ghsync.Sentinels rather than a
// list written out here, so a sentinel added there cannot quietly reach the page
// as "something went wrong".
//
// This is the check that makes splitting ErrRejected and ErrUpstream safe. The
// failure mode being guarded is not a crash: the page still renders, the status
// is still non-200, and the reader is told nothing they can act on. Nothing else
// in the suite would notice.
func TestEveryGithubSentinelHasItsOwnMessage(t *testing.T) {
	tr := testTranslator(t)

	for _, sentinel := range ghsync.Sentinels() {
		// Wrapped, because that is how it always arrives: classify attaches the
		// method, the path and GitHub's own words.
		err := fmt.Errorf("%w: GET /repos/a/b: detail", sentinel)
		key := syncMessageKey(err)

		if key == "error.internal" {
			t.Errorf("%v falls through to error.internal; add a case to syncMessageKey", sentinel)
			continue
		}

		// T returns the key itself when the catalogue has no entry for it, which
		// would put a key name on the page.
		if got := tr.T(key); got == key || got == "" {
			t.Errorf("%v maps to %q, which the catalogue does not have", sentinel, key)
		}
	}
}

// TestSentinelsListsEverySentinelThisFileMaps closes the guard's own blind spot.
//
// The walk above iterates ghsync.Sentinels, so a sentinel missing from that list
// is invisible to it — the check would go quiet rather than red. This holds the
// two lists against each other: the ones mapped here, and the ones the walk
// sees. Neither can lose an entry without the other noticing.
func TestSentinelsListsEverySentinelThisFileMaps(t *testing.T) {
	// Written out rather than derived, because deriving it from Sentinels is
	// exactly the omission this test exists to catch.
	known := []error{
		ghsync.ErrUnauthorized,
		ghsync.ErrForbidden,
		ghsync.ErrNotFound,
		ghsync.ErrRateLimited,
		ghsync.ErrRejected,
		ghsync.ErrUpstream,
		ghsync.ErrInvalid,
	}

	for _, sentinel := range known {
		if !slices.Contains(ghsync.Sentinels(), sentinel) {
			t.Errorf("%v is mapped here but missing from ghsync.Sentinels, "+
				"so the exhaustiveness walk cannot see it", sentinel)
		}
	}

	// A new sentinel listed there but not here has no message written for it
	// yet; the length is what says so.
	if got := len(ghsync.Sentinels()); got != len(known) {
		t.Errorf("ghsync.Sentinels has %d entries, this file maps %d: one of the two is out of date", got, len(known))
	}
}

// TestSyncMessageKeyPinsEachMapping is the other half: the walk above proves
// every sentinel has a message, this proves each one has the right one.
//
// The two ErrNotFound values are the reason this cannot be inferred from the
// sentinel list: store.ErrNotFound means "no token is stored yet", which is the
// same instruction as a rejected token, while ghsync.ErrNotFound means the
// repository or workflow is unknown. Same name, different sentence.
func TestSyncMessageKeyPinsEachMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"token rejected", ghsync.ErrUnauthorized, "sync.error.token"},
		{"no token stored", store.ErrNotFound, "sync.error.token"},
		{"scope missing", ghsync.ErrForbidden, "sync.error.forbidden"},
		{"repo or workflow unknown", ghsync.ErrNotFound, "sync.error.not_found"},
		{"rate limit spent", ghsync.ErrRateLimited, "sync.error.rate_limited"},
		{"request refused", ghsync.ErrRejected, "sync.error.rejected"},
		{"github could not answer", ghsync.ErrUpstream, "sync.error.upstream"},
		{"spec invalid", ghsync.ErrInvalid, "sync.error.spec"},
		{"vault locked", auth.ErrLocked, "error.locked"},
		{"unrecognised", errors.New("boom"), "error.internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncMessageKey(tc.err); got != tc.want {
				t.Errorf("syncMessageKey(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestSyncDetailShowsGithubsOwnWording covers which keys carry the raw error.
//
// The upstream case is new with the split and it is the one that earns its
// place: "bad gateway" is what separates an outage from a bad request, and it is
// the only part of that message which does.
func TestSyncDetailShowsGithubsOwnWording(t *testing.T) {
	err := fmt.Errorf("%w: GET /repos/a/b: bad gateway", ghsync.ErrUpstream)

	for _, key := range []string{
		"sync.error.rejected",
		"sync.error.not_found",
		"sync.error.upstream",
	} {
		if got := syncDetail(err, key); got != err.Error() {
			t.Errorf("syncDetail(%q) = %q, want the raw error", key, got)
		}
	}

	// Keys that carry a fixed instruction must not also carry noise: the reader
	// is being told what to do, and a path they cannot use competes with it.
	for _, key := range []string{
		"sync.error.token",
		"sync.error.forbidden",
		"sync.error.rate_limited",
		"sync.error.spec",
		"error.internal",
	} {
		if got := syncDetail(err, key); got != "" {
			t.Errorf("syncDetail(%q) = %q, want empty", key, got)
		}
	}
}
