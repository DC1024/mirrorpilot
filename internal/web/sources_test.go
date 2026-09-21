package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/DC1024/mirrorpilot/internal/catalog"
	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/probe"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// seedBuiltin puts the shipped catalogue in the database, which is what the
// binary does at startup.
//
// The tests that exercise the mirror pages need it: against an empty table they
// would only ever exercise the "nothing here" branch, which is the one branch
// that cannot be wrong in an interesting way.
func (h *harness) seedBuiltin() {
	h.t.Helper()

	if _, err := catalog.Sync(context.Background(), h.store); err != nil {
		h.t.Fatalf("catalog.Sync: %v", err)
	}
}

// userMirror is a valid submission for the add form.
func userMirror(overrides map[string]string) url.Values {
	form := url.Values{
		"id":       {"my-mirror"},
		"name":     {"My mirror"},
		"url":      {"https://mirror.example.com"},
		"scope":    {"dockerhub"},
		"provider": {"community"},
		"trust":    {"unknown"},
		"enabled":  {"1"},
	}
	for key, value := range overrides {
		if value == "" {
			form.Del(key)
			continue
		}
		form.Set(key, value)
	}
	return form
}

func TestSourcesListsTheBuiltInCatalogue(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	resp := h.get("/sources")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sources = %d, want 200", resp.StatusCode)
	}

	body := h.body(resp)

	for _, want := range []string{
		"DaoCloud",
		"docker.m.daocloud.io",
		"https://www.daocloud.io",
		// The trust badge and the scope tags come from the label tables, so
		// this also proves they resolve to text rather than to a bare key.
		"Verified",
		"Docker Hub",
		// A built-in row is not offered an edit link.
		"Built in",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the mirror list does not mention %q", want)
		}
	}

	// The plain-HTTP mirror carries a warning badge.
	if !strings.Contains(body, "plain HTTP") {
		t.Error("the insecure mirror is not marked as plain HTTP")
	}

	// And an unmeasured mirror says so rather than showing a blank cell.
	if !strings.Contains(body, "never measured") {
		t.Error("an unmeasured mirror does not say so")
	}
}

func TestSourceCreateRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.setup()

	resp := h.post("/sources", userMirror(nil))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /sources = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}
	if got := resp.Header.Get("Location"); got != "/sources?flash=sources.created" {
		t.Errorf("Location = %q", got)
	}

	rec, err := h.store.GetSource(context.Background(), "my-mirror")
	if err != nil {
		t.Fatalf("the mirror was not stored: %v", err)
	}
	if rec.Builtin {
		t.Error("a mirror the user added is marked built-in")
	}
	if !rec.Enabled {
		t.Error("the mirror was stored switched off despite the form saying otherwise")
	}
	if rec.URL != "https://mirror.example.com" {
		t.Errorf("URL = %q", rec.URL)
	}

	if body := h.body(h.get("/sources")); !strings.Contains(body, "My mirror") {
		t.Error("the new mirror is not in the list")
	}
}

// The submission travels back to the form, so a typo costs a correction rather
// than the whole thing.
func TestSourceCreateRejectsWhatItCannotStore(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	t.Run("a duplicate identifier", func(t *testing.T) {
		resp := h.post("/sources", userMirror(map[string]string{"id": "daocloud"}))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST /sources = %d, want 400", resp.StatusCode)
		}

		body := h.body(resp)
		if !strings.Contains(body, "already taken") {
			t.Errorf("the duplicate is not explained; body: %s", body)
		}
		// The value the user typed survived the round trip.
		if !strings.Contains(body, `value="daocloud"`) {
			t.Error("the rejected identifier was not carried back to the form")
		}
	})

	t.Run("an unusable address", func(t *testing.T) {
		resp := h.post("/sources", userMirror(map[string]string{
			"id":  "bad-url",
			"url": "not-a-url",
		}))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST /sources = %d, want 400", resp.StatusCode)
		}

		// The validator's own wording is shown, which is the precise one.
		if body := h.body(resp); !strings.Contains(body, "Reason:") {
			t.Error("the rejection does not give a reason")
		}

		if _, err := h.store.GetSource(context.Background(), "bad-url"); err == nil {
			t.Error("a rejected mirror was stored anyway")
		}
	})

	// An id is a primary key and the form's pattern is a browser convenience,
	// not a check: a hand-made POST has to be refused too.
	t.Run("an identifier the pattern would have caught", func(t *testing.T) {
		resp := h.post("/sources", userMirror(map[string]string{"id": "Not An Id"}))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /sources with a malformed id = %d, want 400", resp.StatusCode)
		}
	})
}

func TestSourceUpdateRewritesAUserMirror(t *testing.T) {
	h := newHarness(t)
	h.setup()

	if resp := h.post("/sources", userMirror(nil)); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}

	form := userMirror(map[string]string{
		"name":    "Renamed",
		"url":     "https://mirror2.example.com",
		"enabled": "",
	})

	resp := h.post("/sources/my-mirror", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /sources/my-mirror = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}
	if got := resp.Header.Get("Location"); got != "/sources?flash=sources.updated" {
		t.Errorf("Location = %q", got)
	}

	rec, err := h.store.GetSource(context.Background(), "my-mirror")
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if rec.Name != "Renamed" || rec.URL != "https://mirror2.example.com" {
		t.Errorf("the row was not rewritten: %+v", rec)
	}
	if rec.Enabled {
		t.Error("unchecking the box did not switch the mirror off")
	}
}

// The id comes from the path, so a form cannot move a row out from under the
// settings and history keyed on it.
func TestSourceUpdateIgnoresTheIdentifierInTheBody(t *testing.T) {
	h := newHarness(t)
	h.setup()

	if resp := h.post("/sources", userMirror(nil)); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}

	h.post("/sources/my-mirror", userMirror(map[string]string{"id": "someone-else"}))

	if _, err := h.store.GetSource(context.Background(), "my-mirror"); err != nil {
		t.Errorf("the row moved: %v", err)
	}
	if _, err := h.store.GetSource(context.Background(), "someone-else"); err == nil {
		t.Error("the body renamed the row")
	}
}

// A built-in's address is not the user's to change; the trust badge depends on
// it meaning what the catalogue says it means.
func TestBuiltInMirrorsRefuseRewriting(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	t.Run("update", func(t *testing.T) {
		resp := h.post("/sources/daocloud", userMirror(map[string]string{
			"id":  "daocloud",
			"url": "https://mine.example.com",
		}))
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /sources/daocloud = %d, want a redirect", resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "/sources?flash=sources.error.builtin_readonly" {
			t.Errorf("Location = %q", got)
		}

		rec, err := h.store.GetSource(context.Background(), "daocloud")
		if err != nil {
			t.Fatalf("GetSource: %v", err)
		}
		if rec.URL != "https://docker.m.daocloud.io" {
			t.Errorf("a built-in mirror's address was rewritten to %q", rec.URL)
		}
	})

	t.Run("delete", func(t *testing.T) {
		resp := h.post("/sources/daocloud/delete", nil)
		if got := resp.Header.Get("Location"); got != "/sources?flash=sources.error.builtin_readonly" {
			t.Errorf("Location = %q", got)
		}
		if _, err := h.store.GetSource(context.Background(), "daocloud"); err != nil {
			t.Errorf("a built-in mirror was deleted: %v", err)
		}
	})

	// Switching one off is the one change it does allow.
	t.Run("but it can be switched off", func(t *testing.T) {
		resp := h.post("/sources/daocloud/enabled", url.Values{"enabled": {"false"}, "next": {"/sources"}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST enabled = %d, want 303", resp.StatusCode)
		}
		if got := resp.Header.Get("Location"); got != "/sources" {
			t.Errorf("Location = %q, want the next it was given", got)
		}

		rec, err := h.store.GetSource(context.Background(), "daocloud")
		if err != nil {
			t.Fatalf("GetSource: %v", err)
		}
		if rec.Enabled {
			t.Error("the mirror is still switched on")
		}
	})

	// And the toggle works from the speed test page too, which is why it takes
	// its destination from the form.
	t.Run("the toggle honours its destination", func(t *testing.T) {
		resp := h.post("/sources/1panel/enabled", url.Values{"enabled": {"false"}, "next": {"/probe"}})
		if got := resp.Header.Get("Location"); got != "/probe" {
			t.Errorf("Location = %q, want /probe", got)
		}
	})
}

func TestSourceDeleteRemovesAUserMirror(t *testing.T) {
	h := newHarness(t)
	h.setup()

	if resp := h.post("/sources", userMirror(nil)); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}

	resp := h.post("/sources/my-mirror/delete", nil)
	if got := resp.Header.Get("Location"); got != "/sources?flash=sources.deleted" {
		t.Errorf("Location = %q", got)
	}

	if _, err := h.store.GetSource(context.Background(), "my-mirror"); err == nil {
		t.Error("the mirror survived its deletion")
	}
}

// A row that is already gone is a flash on the list rather than an error page:
// the list is where the user can see that it is gone.
func TestChangingAMissingMirrorRedirectsWithAReason(t *testing.T) {
	h := newHarness(t)
	h.setup()

	for _, path := range []string{"/sources/nope/delete", "/sources/nope/enabled"} {
		t.Run(path, func(t *testing.T) {
			resp := h.post(path, url.Values{"enabled": {"false"}})
			if got := resp.Header.Get("Location"); got != "/sources?flash=sources.error.not_found" {
				t.Errorf("Location = %q", got)
			}
		})
	}
}

// The edit link prefills the form from the stored row.
func TestEditQueryPrefillsTheForm(t *testing.T) {
	h := newHarness(t)
	h.setup()

	if resp := h.post("/sources", userMirror(map[string]string{"note": "a note"})); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}

	body := h.body(h.get("/sources?edit=my-mirror"))

	for _, want := range []string{
		`value="My mirror"`,
		`value="https://mirror.example.com"`,
		`value="a note"`,
		// An edit posts to the row, and the id is shown but no longer
		// editable — the hint explains why.
		`action="/sources/my-mirror"`,
		"cannot change",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the edit form does not contain %q", want)
		}
	}

	// An edit target that has gone missing falls back to the add form rather
	// than erroring.
	if body := h.body(h.get("/sources?edit=vanished")); !strings.Contains(body, `action="/sources"`) {
		t.Error("a missing edit target did not fall back to the add form")
	}
}

func TestSourcesPageExplainsTheTrustGrade(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	body := h.body(h.get("/sources"))

	for _, want := range []string{
		"What the trust badge means",
		"not a security guarantee",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not explain the trust grade: %q missing", want)
		}
	}
}

// The dashboard counts the catalogue and links to the pages that act on it.
func TestDashboardSummarisesTheCatalogue(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.seedBuiltin()

	builtin, err := mirror.Builtin()
	if err != nil {
		t.Fatalf("mirror.Builtin: %v", err)
	}

	body := h.body(h.get("/dashboard"))

	if !strings.Contains(body, "Mirrors") {
		t.Error("the dashboard has no mirror summary")
	}
	// "never" is the placeholder for a catalogue nothing has measured yet.
	if !strings.Contains(body, "never") {
		t.Error("the dashboard does not say nothing has been measured")
	}
	if !strings.Contains(body, "/sources") || !strings.Contains(body, "/probe") {
		t.Error("the dashboard does not link to the mirror pages")
	}

	// The counts come from the store rather than from the template, so they
	// should match what was seeded.
	rows, err := h.store.ListSources(context.Background())
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != builtin.Len() {
		t.Errorf("the store holds %d mirrors, want %d", len(rows), builtin.Len())
	}
}

// The probe target is a setting, and an empty field means the default rather
// than an empty string.
func TestProbeTargetSettingRoundTrips(t *testing.T) {
	h := newHarness(t)
	h.setup()

	ctx := context.Background()

	if resp := h.post("/settings", url.Values{
		"locale":           {"en"},
		"theme":            {"auto"},
		"probe_repository": {"library/busybox"},
		"probe_reference":  {"1.36"},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /settings = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}

	repository, err := h.store.SettingOrDefault(ctx, store.SettingProbeRepository, "")
	if err != nil {
		t.Fatalf("SettingOrDefault: %v", err)
	}
	if repository != "library/busybox" {
		t.Errorf("stored repository = %q", repository)
	}

	// The speed test page says which image it would measure, which is how the
	// setting becomes visible without running anything.
	if body := h.body(h.get("/probe")); !strings.Contains(body, "library/busybox:1.36") {
		t.Error("the speed test page does not name the configured image")
	}

	// Blanking the fields clears the settings, so the default takes over again.
	if resp := h.post("/settings", url.Values{
		"locale":           {"en"},
		"theme":            {"auto"},
		"probe_repository": {""},
		"probe_reference":  {""},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /settings = %d, want 303", resp.StatusCode)
	}

	fallback := probe.DefaultTarget()
	if body := h.body(h.get("/probe")); !strings.Contains(body, fallback.Repository+":"+fallback.Reference) {
		t.Errorf("clearing the fields did not restore the default %s:%s", fallback.Repository, fallback.Reference)
	}
}
