package i18n

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func newBundle(t *testing.T) *Bundle {
	t.Helper()

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestNewLoadsShippedLocales(t *testing.T) {
	b := newBundle(t)

	if !b.Has("en") {
		t.Error("the default locale is missing")
	}
	if !b.Has("zh-CN") {
		t.Error("the Chinese locale is missing")
	}
	if b.Has("de") {
		t.Error("Has reported a locale that is not shipped")
	}

	langs := b.Langs()
	if len(langs) != 2 {
		t.Fatalf("Langs = %v, want 2 entries", langs)
	}
	if !sort.StringsAreSorted(langs) {
		t.Errorf("Langs = %v, want sorted", langs)
	}
}

// Every locale must define exactly the same keys as the default one.
//
// A key present in English but not Chinese is what produces a half-translated
// page in production, and it is invisible in review because the two files are
// read separately. This is the check that makes the two files one artefact.
func TestKeyParity(t *testing.T) {
	b := newBundle(t)

	want := keySet(b, DefaultLang)
	if len(want) == 0 {
		t.Fatal("the default locale is empty")
	}

	for _, lang := range b.Langs() {
		if lang == DefaultLang {
			continue
		}

		got := keySet(b, lang)

		var missing, extra []string
		for key := range want {
			if !got[key] {
				missing = append(missing, key)
			}
		}
		for key := range got {
			if !want[key] {
				extra = append(extra, key)
			}
		}

		sort.Strings(missing)
		sort.Strings(extra)

		if len(missing) > 0 {
			t.Errorf("%s is missing %d key(s): %s", lang, len(missing), strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			t.Errorf("%s has %d key(s) %s does not: %s", lang, len(extra), DefaultLang, strings.Join(extra, ", "))
		}
	}
}

// An empty translation is almost always a half-finished edit, and it renders as
// a blank label rather than an obvious error.
func TestNoEmptyValues(t *testing.T) {
	b := newBundle(t)

	for _, lang := range b.Langs() {
		for key, value := range b.catalogs[lang] {
			if strings.TrimSpace(value) == "" {
				t.Errorf("%s: %q is empty", lang, key)
			}
		}
	}
}

// Placeholders must agree between locales, or one language renders a literal
// "%d" in front of the user.
func TestPlaceholdersAgree(t *testing.T) {
	b := newBundle(t)

	for key, english := range b.catalogs[DefaultLang] {
		wantVerbs := formatVerbs(english)

		for _, lang := range b.Langs() {
			if lang == DefaultLang {
				continue
			}
			gotVerbs := formatVerbs(b.catalogs[lang][key])
			if strings.Join(gotVerbs, ",") != strings.Join(wantVerbs, ",") {
				t.Errorf("%s: %q has placeholders %v, want %v (from English)",
					lang, key, gotVerbs, wantVerbs)
			}
		}
	}
}

// Keys are flat and namespaced, so a catalogue stays greppable and a typo lands
// in an obviously wrong namespace rather than a plausible one.
func TestKeysAreWellFormed(t *testing.T) {
	b := newBundle(t)

	for _, lang := range b.Langs() {
		for key := range b.catalogs[lang] {
			if strings.ContainsAny(key, " \t\n") {
				t.Errorf("%s: key %q contains whitespace", lang, key)
			}
			if strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") || strings.Contains(key, "..") {
				t.Errorf("%s: key %q has an empty path segment", lang, key)
			}
			if !strings.Contains(key, ".") {
				t.Errorf("%s: key %q is not namespaced", lang, key)
			}
		}
	}
}

// A duplicated key in a locale file is invisible to encoding/json, which keeps
// the last one silently — so a copy-paste slip would ship a string nobody
// intended. Counting keys at the token level is the only way to notice.
func TestNoDuplicateKeysInSource(t *testing.T) {
	b := newBundle(t)

	for _, lang := range b.Langs() {
		name := localeDir + "/" + lang + ".json"

		raw, err := files.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		counted := countObjectKeys(t, name, raw)
		if unique := len(b.catalogs[lang]); counted != unique {
			t.Errorf("%s declares %d keys but only %d are unique; a key is repeated",
				name, counted, unique)
		}
	}
}

// countObjectKeys returns how many key/value pairs a flat JSON object declares,
// counting repeats.
func countObjectKeys(t *testing.T, name string, raw []byte) int {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(raw))

	open, err := dec.Token()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if delim, ok := open.(json.Delim); !ok || delim != '{' {
		t.Fatalf("%s: top level is not an object", name)
	}

	n := 0
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := token.(string); !ok {
			t.Fatalf("%s: expected a key, got %v", name, token)
		}

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n++
	}

	return n
}

func TestTranslate(t *testing.T) {
	b := newBundle(t)

	en := b.Translator("en")
	if got := en.T("nav.dashboard"); got != "Dashboard" {
		t.Errorf("en nav.dashboard = %q", got)
	}

	zh := b.Translator("zh-CN")
	if got := zh.T("nav.dashboard"); got != "总览" {
		t.Errorf("zh-CN nav.dashboard = %q", got)
	}
	if got := zh.Lang(); got != "zh-CN" {
		t.Errorf("Lang = %q, want zh-CN", got)
	}
}

// A key that exists nowhere must render as itself, not as an empty string:
// visible enough to be reported, harmless enough not to break the page.
func TestTranslateUnknownKeyReturnsTheKey(t *testing.T) {
	b := newBundle(t)

	if got := b.Translator("en").T("nope.not.here"); got != "nope.not.here" {
		t.Errorf("T = %q, want the key echoed back", got)
	}
}

func TestTranslateFormatsArguments(t *testing.T) {
	b := newBundle(t)

	got := b.Translator("en").T("setup.password_hint", 8)
	if !strings.Contains(got, "8") {
		t.Errorf("T = %q, want it to contain 8", got)
	}
	if strings.Contains(got, "%d") {
		t.Errorf("T = %q, the placeholder was not filled", got)
	}

	// The same key must format sensibly in every locale, not just in English.
	for _, lang := range b.Langs() {
		localized := b.Translator(lang).T("setup.password_hint", 12)
		if strings.Contains(localized, "%d") {
			t.Errorf("%s: placeholder was not filled: %q", lang, localized)
		}
		if !strings.Contains(localized, "12") {
			t.Errorf("%s: %q does not mention 12", lang, localized)
		}
	}
}

func TestTranslatorFallsBackToTheDefaultLocale(t *testing.T) {
	b := newBundle(t)

	tx := b.Translator("klingon")
	if tx.Lang() != DefaultLang {
		t.Errorf("Lang = %q, want %q", tx.Lang(), DefaultLang)
	}
	if got := tx.T("nav.dashboard"); got != "Dashboard" {
		t.Errorf("T = %q, want the English string", got)
	}
}

// A nil Translator must not panic: templates get built before the bundle is
// always wired up, and a crash on the error page is the worst time for one.
func TestNilTranslatorIsSafe(t *testing.T) {
	var tx *Translator
	if got := tx.T("nav.dashboard"); got != "nav.dashboard" {
		t.Errorf("T on a nil Translator = %q", got)
	}
}

func TestNegotiate(t *testing.T) {
	b := newBundle(t)

	cases := []struct {
		name      string
		preferred string
		header    string
		want      string
	}{
		{"nothing at all", "", "", "en"},
		{"explicit choice", "zh-CN", "", "zh-CN"},
		{"explicit choice, lower case", "zh-cn", "", "zh-CN"},
		{"explicit bare subtag", "zh", "", "zh-CN"},
		{"explicit beats the header", "en", "zh-CN,zh;q=0.9", "en"},
		{"unknown preference falls through to the header", "klingon", "zh-CN", "zh-CN"},
		{"header, simplest", "zh-CN,zh;q=0.9,en;q=0.8", "", "zh-CN"},
		{"header, regional to base", "en-US,en;q=0.9", "", "en"},
		{"header, quality ordering wins", "zh;q=0.3,en;q=0.9", "", "en"},
		{"header, unsupported only", "fr-FR,fr;q=0.9", "", "en"},
		{"header, wildcard", "*", "", "en"},
		{"header, wildcard last", "de;q=0.9,*;q=0.1", "", "en"},
		{"header, q=0 is a refusal", "", "zh-CN;q=0", "en"},
		{"header, q=0 then a real choice", "", "zh-CN;q=0,en;q=0.5", "en"},
		{"header, junk", "", "garbage;;;", "en"},
		{"header, bare quality", "", "q=0.5", "en"},
		{"header, odd casing", "ZH-cn", "", "zh-CN"},
		{"preference padded with spaces", "  zh-CN  ", "", "zh-CN"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.Negotiate(tc.preferred, tc.header); got != tc.want {
				t.Errorf("Negotiate(%q, %q) = %q, want %q", tc.preferred, tc.header, got, tc.want)
			}
		})
	}
}

func TestAcceptLanguageTags(t *testing.T) {
	cases := []struct {
		header string
		want   []string
	}{
		{"", nil},
		{"en", []string{"en"}},
		{"zh-CN,zh;q=0.9,en;q=0.8", []string{"zh-CN", "zh", "en"}},
		{"zh;q=0.3,en;q=0.9", []string{"en", "zh"}},
		{"zh;q=0.9,en;q=0.9", []string{"zh", "en"}},
		{"zh-CN;q=0", nil},
		{"zh-CN;q=0,en", []string{"en"}},
		{"en;q=not-a-number", []string{"en"}},
		{"en;q=0.5;foo=bar", []string{"en"}},
	}

	for _, tc := range cases {
		got := acceptLanguageTags(tc.header)
		if len(got) != len(tc.want) {
			t.Errorf("acceptLanguageTags(%q) = %v, want %v", tc.header, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("acceptLanguageTags(%q) = %v, want %v", tc.header, got, tc.want)
				break
			}
		}
	}
}

func TestMatch(t *testing.T) {
	b := newBundle(t)

	cases := map[string]string{
		"":        "",
		"   ":     "",
		"en":      "en",
		"EN":      "en",
		"en-US":   "en",
		"en-GB":   "en",
		"zh":      "zh-CN",
		"zh-CN":   "zh-CN",
		"zh-cn":   "zh-CN",
		"zh-Hans": "zh-CN",
		"fr":      "",
		"de-DE":   "",
	}

	for tag, want := range cases {
		if got := b.match(tag); got != want {
			t.Errorf("match(%q) = %q, want %q", tag, got, want)
		}
	}
}

// keySet returns the keys of one catalogue as a set.
func keySet(b *Bundle, lang string) map[string]bool {
	catalog := b.catalogs[lang]

	out := make(map[string]bool, len(catalog))
	for key := range catalog {
		out[key] = true
	}
	return out
}

// formatVerbs returns the conversion verbs in a format string.
//
// Deliberately a rough scanner rather than a full fmt parser: it only has to
// notice that two locales disagree about how many placeholders a string has.
// "%%" is treated as a literal, which is the only escape these catalogues use.
func formatVerbs(s string) []string {
	var verbs []string

	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			continue
		}
		if s[i+1] == '%' {
			i++
			continue
		}
		verbs = append(verbs, string(s[i+1]))
		i++
	}

	return verbs
}
