package web

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// preContent returns what a generated document's <pre> block holds, with the
// HTML escaping undone.
//
// The page carries the same text in more than one place — a pasted daemon.json
// is echoed back into its textarea, and an image address into its input — so
// asserting on the whole body cannot tell "the generator produced this" from
// "the form remembered what I typed". Reading the block by its id can.
//
// Unescaped because the assertions are about the document: &#34; is how a quote
// reaches the browser, not what the generator wrote.
func preContent(t *testing.T, body, id string) string {
	t.Helper()

	marker := `<pre class="file" id="` + id + `">`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("the page has no <pre id=%q>", id)
	}
	start += len(marker)

	end := strings.Index(body[start:], "</pre>")
	if end < 0 {
		t.Fatalf("the <pre id=%q> is not closed", id)
	}
	return html.UnescapeString(body[start : start+end])
}

// seedMirrors puts the shipped catalogue in the database and leaves the panel
// signed in, which is the state every test here starts from.
func seedMirrors(t *testing.T) *harness {
	t.Helper()

	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	return h
}

func TestConfigRanksTheEnabledDockerHubMirrors(t *testing.T) {
	h := seedMirrors(t)

	body := h.body(h.get("/config"))

	for _, want := range []string{
		"docker.m.daocloud.io",
		"docker.1panel.live",
		"docker.1ms.run",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the configuration page does not mention the enabled mirror %q", want)
		}
	}

	// A mirror that ships switched off is not a candidate. Writing it into
	// someone's daemon.json would make the enable flag a decoration.
	for _, off := range []string{"hub.rat.dev"} {
		if strings.Contains(body, off) {
			t.Errorf("a switched-off mirror (%s) reached the generated configuration", off)
		}
	}

	daemon := preContent(t, body, "daemon-json")
	if !strings.Contains(daemon, "registry-mirrors") {
		t.Error("the generated daemon.json has no registry-mirrors")
	}
	if !strings.Contains(daemon, "https://docker.m.daocloud.io") {
		t.Errorf("the generated daemon.json does not list an enabled mirror: %s", daemon)
	}

	toml := preContent(t, body, "hosts-toml")
	if !strings.Contains(toml, `server = "https://registry-1.docker.io"`) {
		t.Errorf("the generated hosts.toml has no upstream server: %s", toml)
	}
	if !strings.Contains(toml, `[host."https://docker.m.daocloud.io"]`) {
		t.Errorf("the generated hosts.toml does not list an enabled mirror: %s", toml)
	}
}

// The ranking is the whole value of the file, so a measured mirror has to come
// before an unmeasured one rather than in catalogue order.
func TestConfigPutsMeasuredMirrorsFirst(t *testing.T) {
	h := seedMirrors(t)

	// One measurement, on a mirror the catalogue does not list first.
	if err := h.store.InsertProbe(t.Context(), store.ProbeRecord{
		SourceID:         "1ms",
		StartedAt:        time.Now(),
		Connectivity:     string(probe.StatusOK),
		TokenStatus:      string(probe.StatusOK),
		ManifestStatus:   string(probe.StatusOK),
		ThroughputStatus: string(probe.StatusOK),
		ThroughputBPS:    9_000_000,
		Status:           string(probe.StatusOK),
	}); err != nil {
		t.Fatalf("InsertProbe: %v", err)
	}

	body := h.body(h.get("/config"))
	daemon := preContent(t, body, "daemon-json")

	measured := strings.Index(daemon, "docker.1ms.run")
	unmeasured := strings.Index(daemon, "docker.m.daocloud.io")

	if measured < 0 || unmeasured < 0 {
		t.Fatalf("both mirrors should be in the file: %s", daemon)
	}
	if measured > unmeasured {
		t.Error("an unmeasured mirror was ranked above a measured one")
	}
}

func TestConfigCountsTheMirrorsItCannotConfigure(t *testing.T) {
	h := seedMirrors(t)

	resp := h.post("/sources", userMirror(map[string]string{
		"id":    "ghcr-only",
		"name":  "GHCR only",
		"url":   "https://ghcr.example.com",
		"scope": "ghcr",
	}))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /sources = %d, want 303", resp.StatusCode)
	}

	body := h.body(h.get("/config"))
	if !strings.Contains(body, "1 enabled mirror(s) were left out") {
		t.Errorf("the page does not account for the mirror it left out:\n%s", body)
	}
}

// tableRow returns the <tr> block that holds needle, so an assertion about one
// mirror cannot be satisfied by another mirror's row. The page names the same
// mirror in the table, in the warnings, and in the generated files; only the
// row says what the page claims about it.
func tableRow(t *testing.T, body, needle string) string {
	t.Helper()

	at := strings.Index(body, needle)
	if at < 0 {
		t.Fatalf("the page does not mention %q", needle)
	}

	start := strings.LastIndex(body[:at], "<tr>")
	end := strings.Index(body[at:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatalf("%q is not inside a table row", needle)
	}
	return body[start : at+end]
}

// The whole point of the ranking is that a mirror known to be broken is worse
// than one nobody has got round to trying, and the page has to say which is
// which. Calling a measured failure "never measured" is the page telling the
// reader something it does not know.
func TestConfigCallsAMeasuredFailureUnusableNotUnmeasured(t *testing.T) {
	h := seedMirrors(t)

	// A run that reached the registry and then died part-way: the mirror
	// answered everything, so a rate was left behind, but no image came out.
	if err := h.store.InsertProbe(t.Context(), store.ProbeRecord{
		SourceID:         "1ms",
		StartedAt:        time.Now(),
		Connectivity:     string(probe.StatusOK),
		TokenStatus:      string(probe.StatusOK),
		ManifestStatus:   string(probe.StatusOK),
		ThroughputStatus: string(probe.StatusFailed),
		ThroughputBPS:    250_000,
		Status:           string(probe.StatusFailed),
		Detail:           "blob transfer timed out after 3.7 MB",
	}); err != nil {
		t.Fatalf("InsertProbe: %v", err)
	}

	body := h.body(h.get("/config"))

	row := tableRow(t, body, "docker.1ms.run")
	if !strings.Contains(row, "measured, unusable") {
		t.Errorf("a mirror that was measured and failed is not called unusable:\n%s", row)
	}
	if strings.Contains(row, "never measured") {
		t.Errorf("a mirror that was measured is described as never measured:\n%s", row)
	}
	// The rate the dead transfer left behind must not be offered as this
	// mirror's speed. 250 kB/s is how fast it failed, not how fast it is.
	if strings.Contains(row, "kB/s") {
		t.Errorf("a failed run was given a throughput:\n%s", row)
	}

	if !strings.Contains(body, "was measured and did not work") {
		t.Error("the page does not warn about the mirror it ranked last")
	}

	// And the file agrees with the table: a mirror known to stall goes after
	// the ones nobody has tried.
	daemon := preContent(t, body, "daemon-json")
	if strings.Index(daemon, "docker.1ms.run") < strings.Index(daemon, "docker.m.daocloud.io") {
		t.Errorf("a mirror known not to work was ranked above untried ones:\n%s", daemon)
	}
}

// Throttling is not the mirror's fault and says nothing about whether it
// works, so it must not turn into a verdict either way.
func TestConfigLeavesARateLimitedMirrorUndecided(t *testing.T) {
	h := seedMirrors(t)

	if err := h.store.InsertProbe(t.Context(), store.ProbeRecord{
		SourceID:     "1ms",
		StartedAt:    time.Now(),
		Connectivity: string(probe.StatusRateLimited),
		Status:       string(probe.StatusRateLimited),
		Detail:       "HTTP 429",
	}); err != nil {
		t.Fatalf("InsertProbe: %v", err)
	}

	body := h.body(h.get("/config"))
	row := tableRow(t, body, "docker.1ms.run")

	if !strings.Contains(row, "never measured") {
		t.Errorf("a throttled mirror was not left undecided:\n%s", row)
	}
	if strings.Contains(row, "measured, unusable") {
		t.Errorf("a 429 was turned into a verdict:\n%s", row)
	}
}

func TestConfigMergeCarriesThroughSettingsItDoesNotManage(t *testing.T) {
	h := seedMirrors(t)

	resp := h.post("/config", url.Values{
		"existing": {`{"data-root":"/srv/docker","registry-mirrors":["https://old.example.com"]}`},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /config = %d, want 200", resp.StatusCode)
	}

	daemon := preContent(t, h.body(resp), "daemon-json")

	if !strings.Contains(daemon, "/srv/docker") {
		t.Errorf("a setting this panel does not manage was dropped: %s", daemon)
	}
	if strings.Contains(daemon, "old.example.com") {
		t.Errorf("the previous registry-mirrors survived a replacement: %s", daemon)
	}
	if !strings.Contains(daemon, "docker.m.daocloud.io") {
		t.Errorf("the merged file lost the mirrors it was generated from: %s", daemon)
	}
}

func TestConfigMergeRefusesADocumentItCannotRead(t *testing.T) {
	h := seedMirrors(t)

	// A comment. Docker rejects it, so merging around it would produce a file
	// the reader cannot paste anywhere.
	resp := h.post("/config", url.Values{
		"existing": {"{\n  // the old one had a comment\n  \"data-root\": \"/srv/docker\"\n}"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /config = %d, want 400", resp.StatusCode)
	}

	body := h.body(resp)
	if !strings.Contains(body, "not a daemon.json this panel can merge into") {
		t.Errorf("the refusal does not say what was wrong:\n%s", body)
	}
	// With nothing to merge into, no file is offered at all — half a document
	// would be worse than none.
	if strings.Contains(body, `id="daemon-json"`) {
		t.Error("a document was generated from a daemon.json that could not be read")
	}
}

func TestConfigWithNothingToGenerateFromExplainsItself(t *testing.T) {
	h := newHarness(t)
	h.setup()

	body := h.body(h.get("/config"))

	if !strings.Contains(body, "nothing to generate from") {
		t.Errorf("the page does not explain an empty catalogue:\n%s", body)
	}
	if strings.Contains(body, `id="daemon-json"`) {
		t.Error("an empty registry-mirrors list was offered as a document")
	}
}

func TestRelayEndpointRoundTripsAndBuildsAnAddress(t *testing.T) {
	h := seedMirrors(t)

	resp := h.post("/config/relay", url.Values{
		"relay_endpoint": {"swr.cn-north-4.myhuaweicloud.com/ddn-k8s"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /config/relay = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/config?flash=config.relay.saved" {
		t.Errorf("Location = %q", got)
	}

	body := h.body(h.get("/config?image=alpine:3.21"))

	// Docker Hub's implicit library/ namespace is spelled out, because a relay
	// needs the address it would actually fetch.
	const want = "swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/alpine:3.21"
	if !strings.Contains(body, want) {
		t.Errorf("the built address is not on the page; want %q", want)
	}
	if !strings.Contains(body, "docker pull "+want) {
		t.Error("the pull command is not on the page")
	}
}

func TestRelayEndpointIsRefusedRatherThanStored(t *testing.T) {
	h := seedMirrors(t)

	resp := h.post("/config/relay", url.Values{"relay_endpoint": {"https://"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /config/relay = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(h.body(resp), "not usable") {
		t.Error("the refusal does not say the address was unusable")
	}

	// Not stored, so a later visit does not show an error nobody typed on it.
	if body := h.body(h.get("/config")); strings.Contains(body, `value="https://"`) {
		t.Error("an address the builder refused was kept")
	}
}

func TestRelayClearsTheEndpointWhenTheFieldIsEmptied(t *testing.T) {
	h := seedMirrors(t)

	h.body(h.post("/config/relay", url.Values{"relay_endpoint": {"mirror.example.com"}}))

	resp := h.post("/config/relay", url.Values{"relay_endpoint": {""}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /config/relay = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/config?flash=config.relay.cleared" {
		t.Errorf("Location = %q", got)
	}

	if body := h.body(h.get("/config")); strings.Contains(body, `value="mirror.example.com"`) {
		t.Error("the cleared endpoint is still in the form")
	}
}

func TestRelayWithoutAnEndpointSaysWhatIsMissing(t *testing.T) {
	h := seedMirrors(t)

	body := h.body(h.get("/config?image=alpine"))

	if !strings.Contains(body, "Set the relay address first") {
		t.Errorf("the page does not explain the missing endpoint:\n%s", body)
	}
}

func TestRelayRefusesAnAddressDockerWouldNot(t *testing.T) {
	h := seedMirrors(t)

	h.body(h.post("/config/relay", url.Values{"relay_endpoint": {"mirror.example.com"}}))

	// Uppercase is what Docker rejects, and quietly lowercasing it would
	// produce a document that disagrees with what the reader typed.
	body := h.body(h.get("/config?image=Docker.IO/Library/Alpine"))

	if !strings.Contains(body, "not an image address Docker would accept") {
		t.Errorf("the page accepted an address Docker would not:\n%s", body)
	}
}
