// Package i18n holds the UI strings and picks which language to render them in.
//
// Catalogues are embedded in the binary rather than loaded from disk or fetched
// at runtime. The panel is expected to work in places where the network is the
// problem, so a language pack that needed downloading would be a poor joke.
//
// Strings are flat, dot-separated keys: "sources.trust.builtin". Nested JSON
// would look tidier in the file, but flattening means a missing key is a single
// lookup failure instead of a partly-populated subtree.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed locales/*.json
var files embed.FS

// localeDir is where the catalogues live inside the embedded tree.
const localeDir = "locales"

// DefaultLang is used when nothing else claims the request. English rather than
// Chinese because this is an open-source project: the browser's
// Accept-Language header picks Chinese for a Chinese user anyway, and an
// unknown visitor is better served by the language the README is written in.
const DefaultLang = "en"

// Bundle is the set of loaded catalogues plus the indexes used to match
// language tags.
type Bundle struct {
	catalogs map[string]map[string]string

	// exact maps a lower-cased full tag to its canonical spelling, so
	// "zh-cn" resolves to the "zh-CN" catalogue.
	exact map[string]string

	// base maps a lower-cased primary subtag to a catalogue, so "en-GB"
	// resolves to "en". Populated so that a tag equal to its own base always
	// wins over a more specific sibling.
	base map[string]string
}

// New loads every embedded catalogue.
//
// It fails loudly on a malformed or empty file. A language pack that quietly
// half-loaded would show users a page of raw keys with no obvious cause.
func New() (*Bundle, error) {
	entries, err := files.ReadDir(localeDir)
	if err != nil {
		return nil, fmt.Errorf("i18n: read embedded locales: %w", err)
	}

	b := &Bundle{
		catalogs: make(map[string]map[string]string, len(entries)),
		exact:    make(map[string]string, len(entries)),
		base:     make(map[string]string, len(entries)),
	}

	var tags []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		tag := strings.TrimSuffix(entry.Name(), ".json")

		raw, err := files.ReadFile(localeDir + "/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("i18n: read %s: %w", entry.Name(), err)
		}

		catalog := make(map[string]string)
		if err := json.Unmarshal(raw, &catalog); err != nil {
			return nil, fmt.Errorf("i18n: parse %s: %w", entry.Name(), err)
		}
		if len(catalog) == 0 {
			return nil, fmt.Errorf("i18n: %s contains no strings", entry.Name())
		}

		b.catalogs[tag] = catalog
		tags = append(tags, tag)
	}

	if len(tags) == 0 {
		return nil, fmt.Errorf("i18n: no locale files found")
	}
	if _, ok := b.catalogs[DefaultLang]; !ok {
		return nil, fmt.Errorf("i18n: default locale %q is missing", DefaultLang)
	}

	// Two passes, rather than one, so the base index does not depend on the
	// order ReadDir happened to return. A tag that equals its own base ("en")
	// must beat a regional sibling ("en-GB") for requests that only say "en".
	for _, tag := range tags {
		b.exact[strings.ToLower(tag)] = tag
	}
	for _, tag := range tags {
		lower := strings.ToLower(tag)
		base, _, _ := strings.Cut(lower, "-")

		if _, taken := b.base[base]; !taken || lower == base {
			b.base[base] = tag
		}
	}

	return b, nil
}

// Langs lists the shipped locales, sorted, for building a language switcher.
func (b *Bundle) Langs() []string {
	out := make([]string, 0, len(b.catalogs))
	for tag := range b.catalogs {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// Has reports whether tag names a locale we ship.
//
// Callers use it to reject a stored preference that this build no longer
// carries, instead of rendering a page of raw keys.
func (b *Bundle) Has(tag string) bool {
	_, ok := b.catalogs[tag]
	return ok
}

// Keys lists every translatable key, sorted.
//
// The key set of the default locale is the contract: parity with the other
// locales is enforced by test, so this is the whole catalogue. It exists so the
// web package can check that its templates and handlers actually consume what
// is shipped, in both directions.
func (b *Bundle) Keys() []string {
	catalog := b.catalogs[DefaultLang]

	keys := make([]string, 0, len(catalog))
	for key := range catalog {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// Negotiate picks a language, in order of decreasing authority: an explicit
// choice the user made, the browser's Accept-Language header, then DefaultLang.
func (b *Bundle) Negotiate(preferred, acceptLanguage string) string {
	if lang := b.match(preferred); lang != "" {
		return lang
	}

	for _, candidate := range acceptLanguageTags(acceptLanguage) {
		if candidate == "*" {
			return DefaultLang
		}
		if lang := b.match(candidate); lang != "" {
			return lang
		}
	}

	return DefaultLang
}

// match resolves a single language tag, or returns "" if nothing fits.
func (b *Bundle) match(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return ""
	}
	if canonical, ok := b.exact[tag]; ok {
		return canonical
	}

	base, _, _ := strings.Cut(tag, "-")
	if canonical, ok := b.base[base]; ok {
		return canonical
	}
	return ""
}

// Translator renders strings in one language. It is what the templates hold on
// to: cheap to copy, and it never needs to re-resolve the language per lookup.
type Translator struct {
	bundle *Bundle
	lang   string
}

// Translator returns a translator for lang, falling back to DefaultLang when
// the tag is unknown.
func (b *Bundle) Translator(lang string) *Translator {
	if !b.Has(lang) {
		lang = DefaultLang
	}
	return &Translator{bundle: b, lang: lang}
}

// Lang reports the resolved language tag, for the document's lang attribute.
func (t *Translator) Lang() string {
	return t.lang
}

// T looks up a key.
//
// The fallback chain is deliberate: requested locale, then the default locale,
// then the key itself. A partially translated catalogue therefore degrades to
// English rather than to blank space, and a key that exists nowhere shows up in
// the UI as its own name — ugly enough to get reported, harmless enough not to
// take the page down.
//
// Arguments are formatted with fmt only when supplied, so a string containing a
// literal percent sign is safe as long as it takes no arguments.
func (t *Translator) T(key string, args ...any) string {
	if t == nil || t.bundle == nil {
		return key
	}

	msg, ok := t.bundle.catalogs[t.lang][key]
	if !ok {
		msg, ok = t.bundle.catalogs[DefaultLang][key]
	}
	if !ok {
		return key
	}

	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// acceptLanguageTags parses an Accept-Language header into candidate tags
// ordered by descending quality.
//
// A malformed entry is skipped rather than fatal: a garbled header should fall
// back to the default language, not error out the page.
func acceptLanguageTags(header string) []string {
	type candidate struct {
		tag string
		q   float64
	}

	var candidates []candidate

	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		tag := part
		q := 1.0

		if before, rest, found := strings.Cut(part, ";"); found {
			tag = strings.TrimSpace(before)
			for _, param := range strings.Split(rest, ";") {
				value, ok := strings.CutPrefix(strings.TrimSpace(param), "q=")
				if !ok {
					continue
				}
				// A quality we cannot read leaves the default in place, which
				// keeps the entry rather than dropping it.
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					q = parsed
				}
			}
		}

		// q=0 means "explicitly not acceptable".
		if tag == "" || q <= 0 {
			continue
		}
		candidates = append(candidates, candidate{tag: tag, q: q})
	}

	// Stable, so entries with equal quality keep the order the browser wrote
	// them in — which is itself a ranking.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].q > candidates[j].q
	})

	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.tag)
	}
	return out
}
