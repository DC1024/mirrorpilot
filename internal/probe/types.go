// Package probe measures how well a registry mirror actually performs.
//
// The engine is split in two on purpose:
//
//   - Prober is pure measurement. It owns the four layers, holds no database
//     handle, and is testable against an httptest registry.
//   - Runner supplies the policy around it: which mirrors to probe, how many at
//     once, when to back off, and where results are recorded.
//
// The package imports internal/store so that Runner can record results, while
// internal/store stays free of project packages. That keeps the security-
// relevant persistence code reviewable on its own.
package probe

import (
	"fmt"
	"strings"
	"time"
)

// Status is the outcome of one layer, or of a whole probe.
//
// Rate limiting is deliberately its own value rather than a flavour of failure.
// Anonymous limits on public mirrors are the normal case, not a fault, and
// folding 429 into "down" would make healthy mirrors look broken — which is
// exactly the judgement this tool exists to help with.
type Status string

const (
	// StatusOK means the layer did what it was supposed to.
	StatusOK Status = "ok"

	// StatusUnauthorized means the registry answered but wants credentials.
	// For connectivity this still counts as alive: the host is up and speaking
	// the protocol, which is all layer 1 asks.
	StatusUnauthorized Status = "unauthorized"

	// StatusRateLimited means 429. Alive, but it will not talk to us right now.
	StatusRateLimited Status = "rate_limited"

	// StatusUnreachable means the request never got an answer — DNS, TCP, TLS
	// or a timeout.
	StatusUnreachable Status = "unreachable"

	// StatusFailed means we got an answer and it was wrong: a 5xx, an
	// unparseable body, or content whose digest did not match what was
	// promised.
	StatusFailed Status = "failed"

	// StatusSkipped means the layer does not apply to this mirror, such as a
	// token endpoint that a mirror simply does not implement.
	StatusSkipped Status = "skipped"
)

// Alive reports whether a layer status proves the mirror is answering.
func (s Status) Alive() bool {
	switch s {
	case StatusOK, StatusUnauthorized, StatusRateLimited:
		return true
	default:
		return false
	}
}

// Layer is one measurement step.
type Layer struct {
	Status Status

	// Duration is how long the layer took, kept at full resolution rather
	// than pre-truncated to whole milliseconds.
	//
	// Throughput is computed from it, and a blob that arrives in under a
	// millisecond would otherwise be recorded as zero bytes per second —
	// reporting the fastest possible transfer as no measurement at all. Round
	// only at the point of display, via Millis.
	Duration time.Duration

	// Detail is a short explanation, empty when the layer simply succeeded.
	// It is shown verbatim, so it should not repeat what Status already says.
	Detail string
}

// Millis is the layer's duration in whole milliseconds, for display and for
// storage. Use Duration when computing a rate.
func (l Layer) Millis() int64 { return l.Duration.Milliseconds() }

// Result is one complete four-layer measurement of one mirror.
type Result struct {
	SourceID  string
	StartedAt time.Time

	// Connect is layer 1: GET /v2/.
	Connect Layer

	// Token is layer 2: how long the anonymous pull token took. Its duration is
	// itself a quality signal, which is why it gets its own layer rather than
	// being folded into the manifest fetch.
	Token Layer

	// Manifest is layer 3: time to the first byte of a manifest, plus a digest
	// check on what came back.
	Manifest Layer

	// Throughput is layer 4: pulling a real blob. This is the number that
	// matters most, because a mirror can answer instantly and still be useless.
	Throughput Layer

	// Bytes is how much of the blob was actually read.
	Bytes int64

	// BlobDigestChecked records whether a digest check could be made at all.
	//
	// It is separate from BlobDigestOK because "we could not check" and "we
	// checked and it was wrong" are different claims. A read capped by
	// MaxBlobBytes cannot be hashed to the promised digest, and reporting that
	// as a mismatch would accuse a mirror of serving bad content when the
	// shortage was ours.
	BlobDigestChecked bool

	// BlobDigestOK records whether the downloaded blob hashed to the digest the
	// manifest promised. Only meaningful when BlobDigestChecked is true.
	BlobDigestOK bool

	// ResolvedDigest is the digest of the manifest that was probed. Callers pin
	// it so that later runs measure the same bytes instead of whatever a moving
	// tag now points at.
	ResolvedDigest string

	// Status is the overall verdict.
	Status Status

	// Detail explains a non-OK verdict.
	Detail string
}

// BPS is the measured throughput in bytes per second, or zero when layer 4 did
// not produce a measurement. Zero is not a measured zero.
func (r Result) BPS() int64 {
	if r.Throughput.Duration <= 0 || r.Bytes <= 0 {
		return 0
	}
	// Integer arithmetic: Bytes is capped by MaxBlobBytes, so the
	// multiplication cannot overflow.
	return r.Bytes * int64(time.Second) / int64(r.Throughput.Duration)
}

// Target is the image used for layers 3 and 4.
//
// Both fields matter for honesty about what was measured: a repository without
// a tag would resolve to whatever "latest" means today, and a reference that is
// not a digest can be answered from a mirror's cache rather than from upstream.
type Target struct {
	// Repository is the Docker Hub repository, e.g. "library/alpine".
	Repository string

	// Reference is a tag or a digest. A digest is strongly preferred: it is the
	// only form whose result cannot drift between runs.
	Reference string
}

// validate reports whether the target can be probed.
//
// The checks are deliberately shallow. A repository name has a grammar, and
// re-implementing it here would be a second implementation that is wrong at the
// edges; the mirror is the authority on whether a name exists and answers 404
// when it does not. What is refused is input that cannot name anything at all,
// because that produces a request for a path like "/v2//manifests/latest" whose
// 404 gets recorded as the mirror failing — a confident verdict about the wrong
// thing, which is the failure this project exists to avoid.
func (t Target) validate() error {
	if strings.Trim(t.Repository, " /") == "" {
		return fmt.Errorf("probe target has no repository: %q", t.Repository)
	}
	if strings.TrimSpace(t.Reference) == "" {
		return fmt.Errorf("probe target %q has no reference", t.Repository)
	}
	return nil
}

// DefaultTarget is the image measured when the user has not chosen one.
//
// A small, ubiquitous, multi-architecture official image: every mirror that
// proxies Docker Hub carries it, so a probe compares like with like. It is
// pinned by tag rather than by digest because a digest compiled into a release
// would eventually name something upstream has removed, which would break
// probing for every install at once. The resolved digest is reported per run
// instead, so a reader can still tell exactly which bytes were measured.
func DefaultTarget() Target {
	return Target{Repository: "library/alpine", Reference: "latest"}
}
