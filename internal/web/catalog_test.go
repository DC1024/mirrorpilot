package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DC1024/mirrorpilot/internal/i18n"
)

// literalT matches a translation key written out in a template, as in
// {{ .T "some.key" }}. Computed keys ({{ .T .LabelKey }}) are invisible to it
// and are checked by TestCatalogKeysAreAllReferenced instead.
var literalT = regexp.MustCompile(`\.T\s+"([^"]+)"`)

// TestCatalogKeysAreAllReferenced checks one direction of the catalogue
// contract: nothing is translated that the panel never shows.
//
// The other direction is silence — a dead string looks exactly like a live one
// in review, and the locale files are the easiest place in a project for cruft
// to accumulate. This makes the catalogue self-pruning: delete the last user of
// a key and the test tells you to delete the key.
func TestCatalogKeysAreAllReferenced(t *testing.T) {
	bundle, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	haystack := panelSources(t)

	for _, key := range bundle.Keys() {
		if !strings.Contains(haystack, key) {
			t.Errorf("catalogue key %q is never referenced by the panel; use it or drop it from the locale files", key)
		}
	}
}

// TestTemplateKeysExistInTheCatalog catches the reverse: a template naming a
// key nobody translated.
//
// This one is not cosmetic. An unknown key renders as itself, so the failure
// mode is a button that says "action.save" — visible enough in testing, but
// only if someone happens to look at that page in that language.
func TestTemplateKeysExistInTheCatalog(t *testing.T) {
	bundle, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	known := make(map[string]bool, len(bundle.Keys()))
	for _, key := range bundle.Keys() {
		known[key] = true
	}

	for _, name := range append([]string{"layout", "partials"}, pages...) {
		raw, err := assets.ReadFile("templates/" + name + ".html")
		if err != nil {
			t.Fatalf("read templates/%s.html: %v", name, err)
		}

		for _, match := range literalT.FindAllStringSubmatch(string(raw), -1) {
			if !known[match[1]] {
				t.Errorf("templates/%s.html references the unknown key %q", name, match[1])
			}
		}
	}
}

// TestEveryPageTitleKeyIsKnown checks that each rendered page names a real
// title key.
func TestEveryPageTitleKeyIsKnown(t *testing.T) {
	bundle, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}

	known := make(map[string]bool, len(bundle.Keys()))
	for _, key := range bundle.Keys() {
		known[key] = true
	}

	// The keys handed to newPageData as titleKey.
	titles := []string{
		"setup.title",
		"login.title",
		"unlock.title",
		"dashboard.title",
		"settings.title",
		"password.title",
		"error.title",
	}

	for _, key := range titles {
		if !known[key] {
			t.Errorf("page title key %q is not in the catalogue", key)
		}
	}
}

// panelSources concatenates every source the panel uses to name a key.
//
// Keys reach the catalogue from two places: templates, through {{ .T }}, and
// handlers, which resolve message keys in Go. Both have to count, or the check
// would report every handler-only key as dead.
func panelSources(t *testing.T) string {
	t.Helper()

	var b strings.Builder

	// Templates, read from the embedded copy the binary actually ships.
	err := fs.WalkDir(assets, "templates", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}

		raw, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		b.Write(raw)
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded templates: %v", err)
	}

	// Handlers. Test files are skipped deliberately: a key must not be kept
	// alive by the test that asserts it renders.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b.Write(raw)
		b.WriteString("\n")
	}

	return b.String()
}
