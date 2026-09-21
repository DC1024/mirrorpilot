// Package mirror holds the catalogue of container registry mirrors.
//
// The catalogue ships inside the binary and is never fetched from the network.
// A remotely updatable list of endpoints the user is being told to trust is
// executable configuration by another name, and that is a supply-chain hole
// this project is not willing to open. See the README's non-goals.
//
// The package holds data and validation only: it deliberately has no network
// code, because measuring these mirrors is internal/probe's job and the two
// concerns should be reviewable separately.
package mirror

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// ErrInvalid wraps every validation failure so callers can tell "the user sent
// us nonsense" (HTTP 400) apart from "something broke" (HTTP 500) without
// string matching.
var ErrInvalid = errors.New("invalid mirror source")

// Provider describes who operates a mirror.
//
// This records a fact about the operator, not a judgement about speed or
// quality.
type Provider string

const (
	ProviderOfficial  Provider = "official"
	ProviderCloud     Provider = "cloud"
	ProviderAcademic  Provider = "academic"
	ProviderCompany   Provider = "company"
	ProviderCommunity Provider = "community"
	ProviderPersonal  Provider = "personal"
)

// Providers lists every accepted value.
func Providers() []Provider {
	return []Provider{
		ProviderOfficial, ProviderCloud, ProviderAcademic,
		ProviderCompany, ProviderCommunity, ProviderPersonal,
	}
}

// Trust is how much is publicly known about who runs a mirror.
//
// It is deliberately a statement about transparency rather than a security
// guarantee. A verified mirror can still be slow or down, and listing an
// unknown one is not an accusation — it means the user deserves to know that
// little is published about the operator.
type Trust string

const (
	// TrustVerified is a cloud vendor or established institution with a public
	// operator page.
	TrustVerified Trust = "verified"

	// TrustKnown is a publicly known project with a track record, but without
	// institutional backing.
	TrustKnown Trust = "known"

	// TrustUnknown means there is little or no public information about who
	// operates it.
	TrustUnknown Trust = "unknown"
)

// Trusts lists every accepted value, strongest first.
func Trusts() []Trust { return []Trust{TrustVerified, TrustKnown, TrustUnknown} }

// Scope is an upstream registry that a mirror proxies.
type Scope string

const (
	ScopeDockerHub Scope = "dockerhub"
	ScopeGHCR      Scope = "ghcr"
	ScopeQuay      Scope = "quay"
	ScopeGCR       Scope = "gcr"
	ScopeK8s       Scope = "k8s"
	ScopeMCR       Scope = "mcr"
)

// Scopes lists every accepted value.
func Scopes() []Scope {
	return []Scope{ScopeDockerHub, ScopeGHCR, ScopeQuay, ScopeGCR, ScopeK8s, ScopeMCR}
}

// idPattern keeps ids URL-safe and stable, since they are embedded in form
// values and in the database.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)

// Source is a single registry mirror.
type Source struct {
	// ID is a stable lowercase slug, and the primary key everywhere. A user's
	// enabled/disabled choice is keyed on it, so renaming one is a breaking
	// change to stored state.
	ID string `json:"id"`

	Name     string  `json:"name"`
	URL      string  `json:"url"`
	Homepage string  `json:"homepage,omitempty"`
	Scope    []Scope `json:"scope"`

	Provider Provider `json:"provider"`
	Trust    Trust    `json:"trust"`
	Note     string   `json:"note,omitempty"`

	// Builtin marks a source that arrived with the binary. Built-in sources may
	// be disabled but never deleted: a user who turns one off should still be
	// able to see what they turned off, and the next release may add more.
	Builtin bool `json:"builtin"`

	// Enabled is whether automatic probes include this source. Probing by hand
	// always works regardless.
	Enabled bool `json:"enabled"`

	// Insecure marks a mirror that is only reachable over plain HTTP. We still
	// list these, because measuring one is useful and hiding it would not make
	// it go away — but the transport cannot detect tampering, so the UI has to
	// say so loudly and such sources default to disabled.
	Insecure bool `json:"insecure,omitempty"`
}

// Normalize returns a copy with cosmetic differences removed, so that a source
// arriving from a form and one read from the database compare equal.
func (s Source) Normalize() Source {
	s.ID = strings.ToLower(strings.TrimSpace(s.ID))
	s.Name = strings.TrimSpace(s.Name)
	s.URL = strings.TrimRight(strings.TrimSpace(s.URL), "/")
	s.Homepage = strings.TrimRight(strings.TrimSpace(s.Homepage), "/")
	s.Note = strings.TrimSpace(s.Note)

	scope := make([]Scope, 0, len(s.Scope))
	for _, sc := range s.Scope {
		trimmed := Scope(strings.ToLower(strings.TrimSpace(string(sc))))
		if trimmed != "" && !slices.Contains(scope, trimmed) {
			scope = append(scope, trimmed)
		}
	}
	slices.Sort(scope)
	s.Scope = scope

	return s
}

// Validate reports whether the source is well formed, wrapping every failure in
// ErrInvalid.
func (s Source) Validate() error {
	if !idPattern.MatchString(s.ID) {
		return fmt.Errorf("%w: id %q must be a lowercase slug of 2-31 characters", ErrInvalid, s.ID)
	}
	if s.Name == "" {
		return fmt.Errorf("%w: %s: name is required", ErrInvalid, s.ID)
	}

	u, err := url.Parse(s.URL)
	if err != nil {
		return fmt.Errorf("%w: %s: url %q is not a URL: %w", ErrInvalid, s.ID, s.URL, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: %s: url %q has no host", ErrInvalid, s.ID, s.URL)
	}
	switch u.Scheme {
	case "https":
		if s.Insecure {
			return fmt.Errorf("%w: %s: insecure is set but the url is https", ErrInvalid, s.ID)
		}
	case "http":
		if !s.Insecure {
			return fmt.Errorf(
				"%w: %s: url %q is plain HTTP; set insecure to list it anyway", ErrInvalid, s.ID, s.URL)
		}
	default:
		return fmt.Errorf("%w: %s: url scheme %q must be http or https", ErrInvalid, s.ID, u.Scheme)
	}

	if s.Homepage != "" {
		h, err := url.Parse(s.Homepage)
		if err != nil || h.Host == "" || (h.Scheme != "http" && h.Scheme != "https") {
			return fmt.Errorf("%w: %s: homepage %q is not an http(s) URL", ErrInvalid, s.ID, s.Homepage)
		}
	}

	if len(s.Scope) == 0 {
		return fmt.Errorf("%w: %s: at least one scope is required", ErrInvalid, s.ID)
	}
	for _, sc := range s.Scope {
		if !slices.Contains(Scopes(), sc) {
			return fmt.Errorf("%w: %s: scope %q is not one of %v", ErrInvalid, s.ID, sc, Scopes())
		}
	}

	if !slices.Contains(Providers(), s.Provider) {
		return fmt.Errorf("%w: %s: provider %q is not one of %v", ErrInvalid, s.ID, s.Provider, Providers())
	}
	if !slices.Contains(Trusts(), s.Trust) {
		return fmt.Errorf("%w: %s: trust %q is not one of %v", ErrInvalid, s.ID, s.Trust, Trusts())
	}

	return nil
}

// Host returns the mirror's host, or an empty string if the URL is unusable.
func (s Source) Host() string {
	u, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// ServesDockerHub reports whether the mirror proxies Docker Hub.
//
// This matters because registry-mirrors in daemon.json only ever covers
// Docker Hub — a configured mirror does nothing for ghcr.io or quay.io, which
// users routinely assume otherwise about. Probing is likewise only meaningful
// for mirrors that answer the Docker Hub layout.
func (s Source) ServesDockerHub() bool {
	return slices.Contains(s.Scope, ScopeDockerHub)
}

// Catalog is an ordered, indexed set of sources.
type Catalog struct {
	sources []Source
	byID    map[string]int
}

// New validates every source and indexes the result.
func New(sources []Source) (*Catalog, error) {
	normalized := make([]Source, 0, len(sources))
	byID := make(map[string]int, len(sources))

	for _, s := range sources {
		s = s.Normalize()
		if err := s.Validate(); err != nil {
			return nil, err
		}
		if _, dup := byID[s.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate id %q", ErrInvalid, s.ID)
		}
		byID[s.ID] = len(normalized)
		normalized = append(normalized, s)
	}

	return &Catalog{sources: normalized, byID: byID}, nil
}

// All returns every source, in catalogue order.
func (c *Catalog) All() []Source {
	out := make([]Source, len(c.sources))
	copy(out, c.sources)
	return out
}

// Get returns the source with the given id.
func (c *Catalog) Get(id string) (Source, bool) {
	i, ok := c.byID[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		return Source{}, false
	}
	return c.sources[i], true
}

// Len returns the number of sources.
func (c *Catalog) Len() int { return len(c.sources) }
