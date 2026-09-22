// Package configgen turns mirrors this panel has measured into the
// configuration files that actually use them.
//
// It is a pure package: values in, text out. No store, no network, no files.
// Ranking mirrors and merging a configuration are the parts worth getting
// right, and they are easier to trust when they do not also have to talk to a
// database.
//
// Nothing here writes a file or restarts anything. The panel hands a person a
// document to paste. Editing someone's daemon.json for them is how you break a
// machine you were only asked to advise, and it is why this project's
// non-goals say it is not a registry proxy and not a system configurator.
//
// Callers are expected to pass only mirrors that proxy Docker Hub. That is not
// tidiness: registry-mirrors covers Docker Hub and nothing else, so a mirror
// that only proxies ghcr.io would sit in the file looking useful while doing
// nothing at all for the images the reader actually pulls.
package configgen

import (
	"errors"
	"net/url"
	"sort"
	"strings"
)

// Mirror is one candidate for a configuration file.
type Mirror struct {
	// Name is what the mirror is called, used to name it in a warning.
	//
	// The generated files themselves carry only addresses: daemon.json has no
	// place for a comment, and inventing a key to hold one would produce a
	// file Docker refuses to parse.
	Name string

	// URL is the mirror endpoint as stored, e.g.
	// "https://docker.m.daocloud.io". A trailing slash is tolerated and
	// removed.
	URL string

	// Insecure marks a mirror reachable only over plain HTTP. Docker will not
	// use one unless its host is also listed in insecure-registries, so this
	// flag changes the generated document and not just the commentary.
	Insecure bool

	// BPS is the newest measured throughput in bytes per second, and is only
	// ever something to show. Zero means there is no rate to show, which is
	// not a verdict — see Outcome for that.
	//
	// Deliberately not a verdict, because it cannot be one. Bytes that arrived
	// before a transfer died still divide into a plausible number: a mirror
	// that served 3.7 MB of a 3.85 MB blob and then stalled has a throughput of
	// a few hundred kilobytes per second and cannot deliver an image. Reading
	// "non-zero rate" as "works" is how a broken mirror gets ranked first.
	BPS int64

	// Outcome is the newest measurement's verdict on whether a pull through
	// this mirror would work.
	//
	// Separate from BPS because they answer different questions. BPS says how
	// fast, and is legitimately non-zero for a transfer that never finished;
	// Outcome says whether, and is the only thing the ranking and the warnings
	// are allowed to reason from.
	Outcome Outcome
}

// Outcome is a verdict about a mirror.
type Outcome int

const (
	// OutcomeUnknown means no verdict: never measured, or measured on a run
	// that said nothing either way — a 429 is not the mirror's fault and a
	// probe with no blob to fetch never asked the question.
	OutcomeUnknown Outcome = iota

	// OutcomeWorks means the newest run completed.
	OutcomeWorks

	// OutcomeFails means the newest run did not finish: the host did not
	// answer, refused us, or dropped the transfer part-way.
	OutcomeFails
)

// Works reports whether a pull through this mirror would complete.
//
// Exported so the page describing the generated file asks the same question
// the ranking did. Two implementations of "is this mirror any good" is how a
// table ends up explaining an order the file does not have.
func (m Mirror) Works() bool {
	return m.Outcome == OutcomeWorks
}

// Host returns the mirror's host, or "" when the URL is unusable.
//
// The port is kept when there is one: a mirror on a non-default port is a
// normal thing, and dropping the port would produce a configuration pointing
// at the wrong place.
func (m Mirror) Host() string {
	u, err := url.Parse(strings.TrimSpace(m.URL))
	if err != nil {
		return ""
	}
	return u.Host
}

// Endpoint returns the URL in the form a registry-mirrors entry needs: an
// absolute http(s) URL with no trailing slash.
func (m Mirror) Endpoint() string {
	return strings.TrimRight(strings.TrimSpace(m.URL), "/")
}

// WarningCode names a caveat the panel has to put in front of the reader.
//
// A code rather than a sentence, because this package has no business holding
// user-facing prose: the panel translates these, and the translations live in
// the locale files with every other string.
type WarningCode string

const (
	// WarningUnmeasured marks a mirror going into the file without ever
	// having been measured.
	WarningUnmeasured WarningCode = "unmeasured"

	// WarningUnusable marks a mirror that was measured and did not work, so a
	// pull through it would stall or fail.
	//
	// Its own code rather than a flavour of WarningUnmeasured: "we have not
	// tried this" and "we tried this and it did not work" are different
	// statements, and a page that says the first when it means the second
	// teaches its reader to distrust it.
	WarningUnusable WarningCode = "unusable"

	// WarningInsecure marks a mirror that is only reachable over plain HTTP,
	// or that shares a host with one that is. Using it means accepting that
	// the transport cannot detect tampering.
	WarningInsecure WarningCode = "insecure"

	// WarningDuplicated marks mirrors pointing at the same host. Docker tries
	// them in order, so a repeat is pure latency.
	WarningDuplicated WarningCode = "duplicated"
)

// Warning is one caveat about the mirrors that were selected.
type Warning struct {
	Code WarningCode

	// Names are the mirrors the caveat is about, in the order they appear in
	// the generated list.
	Names []string
}

var (
	// ErrNoMirrors means nothing usable was selected. Producing an empty
	// registry-mirrors list instead would look like a successful answer while
	// quietly configuring nothing.
	ErrNoMirrors = errors.New("configgen: no mirrors to configure")

	// ErrExisting means the supplied current configuration could not be read.
	// Refusing to continue is deliberate: emitting a document that silently
	// dropped a setting somebody had already made is worse than emitting
	// nothing.
	ErrExisting = errors.New("configgen: the current configuration cannot be read")
)

// Select ranks mirrors and reports what is worth saying about them.
//
// Ordering is by measured throughput, best first, within three standings:
// mirrors whose newest run worked, mirrors nobody has tried, and mirrors whose
// newest run did not work.
// Docker tries registry-mirrors in the order they appear and stops at the
// first that answers, so the order is the whole value of the list: a fast
// mirror placed second is a slow mirror.
//
// Duplicates are dropped, because a host listed twice costs a round trip and
// buys nothing. Everything else is kept, including the mirrors with caveats —
// the reader is told about the caveat rather than having the choice made for
// them.
func Select(mirrors []Mirror) ([]Mirror, []Warning) {
	seen := make(map[string]bool, len(mirrors))
	kept := make([]Mirror, 0, len(mirrors))
	var duplicated []string

	for _, m := range mirrors {
		host := strings.ToLower(m.Host())
		if host == "" {
			// Not an address, so not something to write into a file. It was
			// rejected when it was added; this is the second line of defence.
			continue
		}
		if seen[host] {
			duplicated = append(duplicated, describe(m))
			continue
		}
		seen[host] = true
		kept = append(kept, m)
	}

	// Stable, so mirrors with the same standing keep the order the catalogue
	// gave them rather than being shuffled by the sort.
	sort.SliceStable(kept, func(i, j int) bool {
		if rank(kept[i]) != rank(kept[j]) {
			return rank(kept[i]) < rank(kept[j])
		}
		return kept[i].BPS > kept[j].BPS
	})

	// Computed after ranking, so the two lists read in the same order as the
	// generated document.
	insecureHost := make(map[string]bool, len(kept))
	for _, m := range kept {
		if m.Insecure {
			insecureHost[strings.ToLower(m.Host())] = true
		}
	}

	var unusable, unmeasured, insecure []string
	for _, m := range kept {
		switch {
		case m.Outcome == OutcomeFails:
			unusable = append(unusable, describe(m))
		case !m.Works():
			unmeasured = append(unmeasured, describe(m))
		}
		// A host is insecure if any mirror on it is. The flag describes the
		// endpoint, not the catalogue row that happened to mention it, and the
		// host ends up in insecure-registries either way.
		if insecureHost[strings.ToLower(m.Host())] {
			insecure = append(insecure, describe(m))
		}
	}

	// The known-broken go first: they are the ones a reader can act on, and
	// they are the ones whose presence in the file costs a real timeout.
	var warnings []Warning
	if len(unusable) > 0 {
		warnings = append(warnings, Warning{Code: WarningUnusable, Names: unusable})
	}
	if len(unmeasured) > 0 {
		warnings = append(warnings, Warning{Code: WarningUnmeasured, Names: unmeasured})
	}
	if len(insecure) > 0 {
		warnings = append(warnings, Warning{Code: WarningInsecure, Names: insecure})
	}
	if len(duplicated) > 0 {
		warnings = append(warnings, Warning{Code: WarningDuplicated, Names: duplicated})
	}

	return kept, warnings
}

// rank orders a mirror for the generated file: those that work first, then
// those nobody has tried, then those known not to.
//
// The last two are the interesting decision. An unmeasured mirror might work
// and a failed one is known not to, so the failed one goes last: Docker tries
// the list in order, and putting a mirror known to stall ahead of one that has
// merely not been tried spends a real pull's patience on the wrong candidate.
func rank(m Mirror) int {
	switch {
	case m.Works():
		return 0
	case m.Outcome == OutcomeFails:
		return 2
	default:
		return 1
	}
}

// endpoints renders the ranked mirrors as registry-mirrors entries.
func endpoints(ranked []Mirror) []string {
	out := make([]string, 0, len(ranked))
	for _, m := range ranked {
		out = append(out, m.Endpoint())
	}
	return out
}

// insecureHosts lists the hosts that have to appear in insecure-registries,
// once each, in the order the ranked list first mentions them.
func insecureHosts(ranked []Mirror) []string {
	isInsecure := make(map[string]bool, len(ranked))
	for _, m := range ranked {
		if m.Insecure {
			isInsecure[strings.ToLower(m.Host())] = true
		}
	}

	var out []string
	emitted := make(map[string]bool, len(ranked))

	for _, m := range ranked {
		host := strings.ToLower(m.Host())
		if host == "" || !isInsecure[host] || emitted[host] {
			continue
		}
		emitted[host] = true
		out = append(out, m.Host())
	}
	return out
}

// describe names a mirror for a warning, falling back to its address when it
// has no name.
func describe(m Mirror) string {
	if name := strings.TrimSpace(m.Name); name != "" {
		return name
	}
	return m.Endpoint()
}
