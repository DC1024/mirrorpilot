package configgen

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mirror(name, url string, bps int64) Mirror {
	return Mirror{Name: name, URL: url, BPS: bps}
}

func TestHostAndEndpointIgnoreATrailingSlash(t *testing.T) {
	m := mirror("x", "https://mirror.example/", 0)

	if got := m.Host(); got != "mirror.example" {
		t.Errorf("Host() = %q, want %q", got, "mirror.example")
	}
	if got := m.Endpoint(); got != "https://mirror.example" {
		t.Errorf("Endpoint() = %q, want %q", got, "https://mirror.example")
	}
}

func TestHostKeepsANonDefaultPort(t *testing.T) {
	// Dropping the port would produce a configuration pointing somewhere
	// else entirely, which is worse than refusing to guess.
	m := mirror("x", "https://mirror.example:5000", 0)

	if got := m.Host(); got != "mirror.example:5000" {
		t.Errorf("Host() = %q, want %q", got, "mirror.example:5000")
	}
}

func TestHostIsEmptyForAnUnusableURL(t *testing.T) {
	for _, url := range []string{"", "   ", "/just/a/path", "not a url at all"} {
		if got := mirror("x", url, 0).Host(); got != "" {
			t.Errorf("Host(%q) = %q, want empty", url, got)
		}
	}
}

func TestSelectOrdersByMeasuredThroughput(t *testing.T) {
	ranked, _ := Select([]Mirror{
		mirror("slow", "https://slow.example", 100),
		mirror("fast", "https://fast.example", 9_000),
		mirror("middle", "https://middle.example", 5_000),
	})

	want := []string{"fast", "middle", "slow"}
	if got := names(ranked); !equal(got, want) {
		t.Errorf("ranked order = %v, want %v", got, want)
	}
}

func TestSelectPutsUnmeasuredMirrorsLastInCatalogueOrder(t *testing.T) {
	// Relative order among the unmeasured ones is not a ranking, it is the
	// order they were given in — anything else would shuffle the list on
	// every render.
	ranked, warnings := Select([]Mirror{
		mirror("never-a", "https://a.example", 0),
		mirror("measured", "https://m.example", 10),
		mirror("never-b", "https://b.example", 0),
	})

	want := []string{"measured", "never-a", "never-b"}
	if got := names(ranked); !equal(got, want) {
		t.Errorf("ranked order = %v, want %v", got, want)
	}
	if len(warnings) != 1 || warnings[0].Code != WarningUnmeasured {
		t.Fatalf("warnings = %+v, want one unmeasured warning", warnings)
	}
	if !equal(warnings[0].Names, []string{"never-a", "never-b"}) {
		t.Errorf("unmeasured names = %v", warnings[0].Names)
	}
}

func TestSelectDropsDuplicatesAndUnusableAddresses(t *testing.T) {
	ranked, warnings := Select([]Mirror{
		mirror("first", "https://mirror.example", 500),
		mirror("same-host-again", "HTTPS://Mirror.example", 900),
		mirror("no-address", "", 100),
	})

	if got := names(ranked); !equal(got, []string{"first"}) {
		t.Errorf("ranked = %v, want just the first entry", got)
	}

	var found bool
	for _, w := range warnings {
		if w.Code == WarningDuplicated {
			found = true
			if !equal(w.Names, []string{"same-host-again"}) {
				t.Errorf("duplicate names = %v", w.Names)
			}
		}
	}
	if !found {
		t.Errorf("warnings = %+v, want a duplicate warning", warnings)
	}
}

func TestSelectWarnsAboutAnInsecureHost(t *testing.T) {
	plain := mirror("plain", "http://plain.example", 300)
	plain.Insecure = true

	ranked, warnings := Select([]Mirror{
		mirror("secure", "https://secure.example", 700),
		plain,
	})

	if got := names(ranked); !equal(got, []string{"secure", "plain"}) {
		t.Fatalf("ranked = %v, want both mirrors", got)
	}

	var insecure []string
	for _, w := range warnings {
		if w.Code == WarningInsecure {
			insecure = w.Names
		}
	}
	if !equal(insecure, []string{"plain"}) {
		t.Errorf("insecure names = %v, want [plain]", insecure)
	}
}

func TestSelectKeepsTheHTTPSRowWhenAHostAppearsTwice(t *testing.T) {
	// A host listed once over https and once over plain HTTP is one endpoint,
	// and the secure spelling of it is the one worth writing into a file.
	plain := mirror("plain", "http://shared.example", 900)
	plain.Insecure = true

	ranked, warnings := Select([]Mirror{
		mirror("secure", "https://shared.example", 100),
		plain,
	})

	if got := names(ranked); !equal(got, []string{"secure"}) {
		t.Fatalf("ranked = %v, want the https row", got)
	}
	for _, w := range warnings {
		if w.Code == WarningInsecure {
			t.Errorf("an https endpoint was reported as insecure: %+v", w)
		}
	}
}

func TestDaemonJSONRendersAFreshConfiguration(t *testing.T) {
	got, warnings, err := DaemonJSON([]Mirror{
		mirror("daocloud", "https://docker.m.daocloud.io", 1_000_000),
		mirror("1panel", "https://docker.1panel.live", 2_900_000),
	}, "")
	if err != nil {
		t.Fatalf("DaemonJSON: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}

	want := `{
  "registry-mirrors": [
    "https://docker.1panel.live",
    "https://docker.m.daocloud.io"
  ]
}
`
	if got != want {
		t.Errorf("DaemonJSON =\n%s\nwant\n%s", got, want)
	}
}

func TestDaemonJSONAddsInsecureRegistriesForPlainHTTPMirrors(t *testing.T) {
	plain := mirror("plain", "http://mirror.example:5000", 10)
	plain.Insecure = true

	got, warnings, err := DaemonJSON([]Mirror{plain}, "")
	if err != nil {
		t.Fatalf("DaemonJSON: %v", err)
	}

	// Docker refuses to use a mirror it does not know is insecure, so without
	// this the generated file would be a config that never takes effect.
	want := `{
  "registry-mirrors": [
    "http://mirror.example:5000"
  ],
  "insecure-registries": [
    "mirror.example:5000"
  ]
}
`
	if got != want {
		t.Errorf("DaemonJSON =\n%s\nwant\n%s", got, want)
	}

	var sawInsecure bool
	for _, w := range warnings {
		sawInsecure = sawInsecure || w.Code == WarningInsecure
	}
	if !sawInsecure {
		t.Errorf("warnings = %+v, want an insecure warning", warnings)
	}
}

func TestDaemonJSONKeepsEverythingItDoesNotManage(t *testing.T) {
	existing := `{
  "log-driver": "json-file",
  "registry-mirrors": ["https://stale.example"],
  "insecure-registries": ["legacy.example:5000"],
  "features": { "buildkit": true }
}`

	plain := mirror("plain", "http://plain.example", 20)
	plain.Insecure = true

	got, _, err := DaemonJSON([]Mirror{
		mirror("fast", "https://fast.example", 900),
		plain,
	}, existing)
	if err != nil {
		t.Fatalf("DaemonJSON: %v", err)
	}

	want := `{
  "log-driver": "json-file",
  "registry-mirrors": [
    "https://fast.example",
    "http://plain.example"
  ],
  "insecure-registries": [
    "legacy.example:5000",
    "plain.example"
  ],
  "features": {
    "buildkit": true
  }
}
`
	if got != want {
		t.Errorf("DaemonJSON =\n%s\nwant\n%s", got, want)
	}
}

func TestDaemonJSONReplacesRegistryMirrorsRatherThanAppending(t *testing.T) {
	got, _, err := DaemonJSON(
		[]Mirror{mirror("only", "https://only.example", 1)},
		`{"registry-mirrors": ["https://stale.example", "https://older.example"]}`,
	)
	if err != nil {
		t.Fatalf("DaemonJSON: %v", err)
	}

	if strings.Contains(got, "stale.example") || strings.Contains(got, "older.example") {
		t.Errorf("DaemonJSON kept the previous mirrors:\n%s", got)
	}
}

func TestDaemonJSONRefusesAnExistingDocumentItCannotRead(t *testing.T) {
	// Comments and trailing commas are the two mistakes daemon.json actually
	// attracts, and Docker rejects both. Guessing at a repair would mean
	// handing back a document that quietly differed from the one supplied.
	for name, existing := range map[string]string{
		"comment":          "{\n  // mirrors\n  \"log-driver\": \"json-file\"\n}",
		"trailing comma":   `{"log-driver": "json-file",}`,
		"top level array":  `["https://mirror.example"]`,
		"bare number":      `42`,
		"two documents":    `{"a":1} {"b":2}`,
		"wrong list shape": `{"insecure-registries": "legacy.example"}`,
	} {
		mirrors := []Mirror{mirror("ok", "https://ok.example", 1)}
		if name == "wrong list shape" {
			plain := mirror("plain", "http://plain.example", 1)
			plain.Insecure = true
			mirrors = []Mirror{plain}
		}

		_, _, err := DaemonJSON(mirrors, existing)
		if !errors.Is(err, ErrExisting) {
			t.Errorf("%s: err = %v, want ErrExisting", name, err)
		}
	}
}

func TestDaemonJSONRefusesToGenerateNothing(t *testing.T) {
	// An empty registry-mirrors list is a valid document that configures
	// nothing, which reads as success. Saying so is the honest answer.
	if _, _, err := DaemonJSON(nil, ""); !errors.Is(err, ErrNoMirrors) {
		t.Errorf("err = %v, want ErrNoMirrors", err)
	}
	if _, _, err := DaemonJSON([]Mirror{mirror("broken", "", 5)}, ""); !errors.Is(err, ErrNoMirrors) {
		t.Errorf("err = %v, want ErrNoMirrors for an unusable address", err)
	}
}

func TestDaemonJSONOutputIsAlwaysValidJSON(t *testing.T) {
	got, _, err := DaemonJSON([]Mirror{mirror("only", "https://only.example", 1)}, "")
	if err != nil {
		t.Fatalf("DaemonJSON: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
}

func TestContainerdHostsTOMLRendersTheMirrorsInOrder(t *testing.T) {
	got, _, err := ContainerdHostsTOML([]Mirror{
		mirror("slow", "https://slow.example", 100),
		mirror("fast", "https://fast.example", 5_000),
	})
	if err != nil {
		t.Fatalf("ContainerdHostsTOML: %v", err)
	}

	// Order is the entire value of the file, so it is asserted directly rather
	// than through a substring count.
	fast := strings.Index(got, `[host."https://fast.example"]`)
	slow := strings.Index(got, `[host."https://slow.example"]`)
	if fast < 0 || slow < 0 {
		t.Fatalf("a mirror is missing from the output:\n%s", got)
	}
	if fast > slow {
		t.Errorf("the faster mirror is listed second:\n%s", got)
	}
	if !strings.Contains(got, `server = "https://registry-1.docker.io"`) {
		t.Errorf("the upstream server is missing:\n%s", got)
	}
	if !strings.Contains(got, `capabilities = ["pull", "resolve"]`) {
		t.Errorf("host capabilities are missing:\n%s", got)
	}
	if !strings.Contains(got, "/etc/containerd/certs.d") {
		t.Errorf("the destination instructions are missing:\n%s", got)
	}
}

func TestContainerdHostsTOMLRefusesToGenerateNothing(t *testing.T) {
	if _, _, err := ContainerdHostsTOML(nil); !errors.Is(err, ErrNoMirrors) {
		t.Errorf("err = %v, want ErrNoMirrors", err)
	}
}

func names(mirrors []Mirror) []string {
	out := make([]string, 0, len(mirrors))
	for _, m := range mirrors {
		out = append(out, m.Name)
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
