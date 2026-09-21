package mirror

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// validSource is a known-good baseline that individual cases then break.
func validSource() Source {
	return Source{
		ID:       "example",
		Name:     "Example",
		URL:      "https://example.invalid",
		Homepage: "https://example.invalid/about",
		Scope:    []Scope{ScopeDockerHub},
		Provider: ProviderCommunity,
		Trust:    TrustKnown,
		Enabled:  true,
	}
}

func TestBuiltinCatalogueLoads(t *testing.T) {
	catalog, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}
	if catalog.Len() == 0 {
		t.Fatal("built-in catalogue is empty")
	}

	var enabledServingHub int
	for _, s := range catalog.All() {
		if !s.Builtin {
			t.Errorf("%s: built-in catalogue entry is not marked builtin", s.ID)
		}
		if s.Name == "" || s.Note == "" {
			t.Errorf("%s: name and note are required so the UI can describe it", s.ID)
		}
		if s.Enabled && s.ServesDockerHub() {
			enabledServingHub++
		}
	}
	if enabledServingHub == 0 {
		t.Error("no enabled source serves Docker Hub, so a fresh install would have nothing to probe")
	}
}

// A mirror that cannot authenticate its transport must not be probed
// automatically. Otherwise the tool quietly normalises plain HTTP.
func TestBuiltinInsecureSourcesAreDisabled(t *testing.T) {
	catalog, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}

	for _, s := range catalog.All() {
		if s.Insecure && s.Enabled {
			t.Errorf("%s: insecure source is enabled by default", s.ID)
		}
		if strings.HasPrefix(s.URL, "http://") && !s.Insecure {
			t.Errorf("%s: plain HTTP source is not flagged insecure", s.ID)
		}
	}
}

func TestBuiltinIdsAreStableSlugs(t *testing.T) {
	catalog, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}

	// Ids are keys in the database and in form posts. An id that is not a plain
	// lowercase slug tends to break one of those quietly.
	for _, s := range catalog.All() {
		if !idPattern.MatchString(s.ID) {
			t.Errorf("%s: id is not a lowercase slug", s.ID)
		}
	}
}

func TestNormalize(t *testing.T) {
	got := Source{
		ID:       "  Example  ",
		Name:     "  Example  ",
		URL:      "https://example.invalid/",
		Homepage: "https://example.invalid/about/",
		Scope:    []Scope{" DockerHub ", "ghcr", "ghcr", ""},
		Note:     "  spaced  ",
	}.Normalize()

	want := Source{
		ID:       "example",
		Name:     "Example",
		URL:      "https://example.invalid",
		Homepage: "https://example.invalid/about",
		Scope:    []Scope{ScopeDockerHub, ScopeGHCR},
		Note:     "spaced",
	}

	if got.ID != want.ID || got.Name != want.Name || got.URL != want.URL {
		t.Errorf("Normalize() = %+v, want %+v", got, want)
	}
	if got.Homepage != want.Homepage || got.Note != want.Note {
		t.Errorf("Normalize() = %+v, want %+v", got, want)
	}
	if !slices.Equal(got.Scope, want.Scope) {
		t.Errorf("Normalize() scope = %v, want %v", got.Scope, want.Scope)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Source)
		wantErr bool
	}{
		{"baseline", func(*Source) {}, false},
		{"no homepage", func(s *Source) { s.Homepage = "" }, false},
		{"http with insecure", func(s *Source) {
			s.URL = "http://example.invalid"
			s.Insecure = true
		}, false},
		{"empty id", func(s *Source) { s.ID = "" }, true},
		{"uppercase id", func(s *Source) { s.ID = "Example" }, true},
		{"id with underscore", func(s *Source) { s.ID = "some_example" }, true},
		{"one character id", func(s *Source) { s.ID = "a" }, true},
		{"empty name", func(s *Source) { s.Name = "" }, true},
		{"empty url", func(s *Source) { s.URL = "" }, true},
		{"url without host", func(s *Source) { s.URL = "https://" }, true},
		{"plain http not flagged", func(s *Source) { s.URL = "http://example.invalid" }, true},
		{"insecure but https", func(s *Source) { s.Insecure = true }, true},
		{"unsupported scheme", func(s *Source) { s.URL = "ftp://example.invalid" }, true},
		{"bad homepage", func(s *Source) { s.Homepage = "not a url" }, true},
		{"no scope", func(s *Source) { s.Scope = nil }, true},
		{"bad scope", func(s *Source) { s.Scope = []Scope{"docker.io"} }, true},
		{"bad provider", func(s *Source) { s.Provider = "vendor" }, true},
		{"empty provider", func(s *Source) { s.Provider = "" }, true},
		{"bad trust", func(s *Source) { s.Trust = "trusted" }, true},
		{"empty trust", func(s *Source) { s.Trust = "" }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validSource()
			tc.mutate(&s)

			err := s.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() error = %v, want it to wrap ErrInvalid", err)
			}
		})
	}
}

func TestNewRejectsDuplicates(t *testing.T) {
	a, b := validSource(), validSource()
	b.Name = "Example Two"

	if _, err := New([]Source{a, b}); err == nil {
		t.Fatal("New() = nil, want a duplicate-id error")
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("New() error = %v, want it to wrap ErrInvalid", err)
	}
}

func TestNewNormalizesBeforeIndexing(t *testing.T) {
	s := validSource()
	s.ID = "  Example  "

	catalog, err := New([]Source{s})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Lookups must work whether or not the caller got the casing right, since
	// ids arrive from form fields and URLs.
	for _, id := range []string{"example", "Example", "  EXAMPLE  "} {
		if _, ok := catalog.Get(id); !ok {
			t.Errorf("Get(%q) not found", id)
		}
	}
	if _, ok := catalog.Get("missing"); ok {
		t.Error("Get(missing) found something")
	}
}

func TestAllReturnsACopy(t *testing.T) {
	catalog, err := New([]Source{validSource()})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first := catalog.All()
	first[0].Name = "mutated"

	if catalog.All()[0].Name == "mutated" {
		t.Error("All() exposed the catalogue's internal slice")
	}
}

func TestSourceHelpers(t *testing.T) {
	s := validSource()
	if got := s.Host(); got != "example.invalid" {
		t.Errorf("Host() = %q", got)
	}
	if !s.ServesDockerHub() {
		t.Error("ServesDockerHub() = false for a dockerhub-scoped source")
	}

	ghcrOnly := validSource()
	ghcrOnly.Scope = []Scope{ScopeGHCR}
	if ghcrOnly.ServesDockerHub() {
		t.Error("ServesDockerHub() = true for a ghcr-only source")
	}

	broken := Source{URL: "://nope"}
	if got := broken.Host(); got != "" {
		t.Errorf("Host() on an unparseable URL = %q, want empty", got)
	}
}
