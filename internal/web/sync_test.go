package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/ghsync"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// syncForm is a complete, valid submission for the relocation settings.
func syncForm(overrides map[string]string) url.Values {
	form := url.Values{
		"owner":        {"DC1024"},
		"repo":         {"image-relay"},
		"workflow":     {"mirrorpilot-relay.yml"},
		"ref":          {"main"},
		"registry":     {"registry.cn-hangzhou.aliyuncs.com"},
		"namespace":    {"dc-images"},
		"github_token": {"ghp_example"},
		"acr_password": {"hunter2"},
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

func TestSyncPageOffersTheWorkflowBeforeAnythingIsConfigured(t *testing.T) {
	h := newHarness(t)
	h.setup()

	body := h.body(h.get("/sync"))

	for _, want := range []string{
		"mirrorpilot-relay.yml",
		"workflow_dispatch:",
		"ACR_USERNAME",
		"ACR_PASSWORD",
		"docker pull",
		"Not ready to dispatch",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the relocation page does not mention %q", want)
		}
	}
}

func TestSyncSettingsRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.setup()

	resp := h.post("/sync/settings", syncForm(nil))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /sync/settings = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}
	if got := resp.Header.Get("Location"); got != "/sync?flash=sync.saved" {
		t.Errorf("Location = %q", got)
	}

	body := h.body(h.get("/sync"))
	for _, want := range []string{
		"DC1024", "image-relay", "registry.cn-hangzhou.aliyuncs.com", "dc-images",
		// Both credential fields report a stored value...
		"A value is stored",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the saved settings do not show %q", want)
		}
	}

	// ...and neither of them renders the value, which the panel has no
	// business putting back on a screen.
	for _, secret := range []string{"ghp_example", "hunter2"} {
		if strings.Contains(body, secret) {
			t.Errorf("a stored credential was rendered into the page: %q", secret)
		}
	}

	stored, err := h.auth.ConfiguredCredentials(t.Context())
	if err != nil {
		t.Fatalf("ConfiguredCredentials: %v", err)
	}
	if !stored[store.CredentialGitHubToken] {
		t.Error("the GitHub token was not stored")
	}
	if !stored[store.CredentialACRPassword] {
		t.Error("the registry password was not stored")
	}
}

// The form cannot show what is already stored, so a blank credential field has
// to mean "unchanged" — otherwise saving the repository would wipe the token.
func TestSyncSettingsLeavesABlankCredentialAlone(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.body(h.post("/sync/settings", syncForm(nil)))

	h.body(h.post("/sync/settings", syncForm(map[string]string{
		"repo":         "other-repo",
		"github_token": "",
		"acr_password": "",
	})))

	stored, err := h.auth.ConfiguredCredentials(t.Context())
	if err != nil {
		t.Fatalf("ConfiguredCredentials: %v", err)
	}
	if !stored[store.CredentialGitHubToken] || !stored[store.CredentialACRPassword] {
		t.Error("saving the unrelated fields removed a stored credential")
	}
}

func TestSyncSettingsRemovesACredentialOnRequest(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.body(h.post("/sync/settings", syncForm(nil)))

	h.body(h.post("/sync/settings", syncForm(map[string]string{
		"github_token":       "",
		"clear_github_token": "1",
	})))

	stored, err := h.auth.ConfiguredCredentials(t.Context())
	if err != nil {
		t.Fatalf("ConfiguredCredentials: %v", err)
	}
	if stored[store.CredentialGitHubToken] {
		t.Error("the GitHub token was not removed")
	}
	if !stored[store.CredentialACRPassword] {
		t.Error("removing one credential removed the other")
	}
}

func TestSyncSettingsRefusesHalfATypedRepository(t *testing.T) {
	h := newHarness(t)
	h.setup()

	resp := h.post("/sync/settings", syncForm(map[string]string{"repo": ""}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /sync/settings = %d, want 400", resp.StatusCode)
	}

	body := h.body(resp)
	if !strings.Contains(body, "not set up properly") {
		t.Errorf("the refusal does not say what was wrong:\n%s", body)
	}
	// What was typed survives the refusal, so the mistake costs one field.
	if !strings.Contains(body, "DC1024") {
		t.Error("the rejected form was not echoed back")
	}
}

func TestSyncSettingsRefusesAHalfTypedTarget(t *testing.T) {
	h := newHarness(t)
	h.setup()

	resp := h.post("/sync/settings", syncForm(map[string]string{"namespace": ""}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /sync/settings = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(h.body(resp), "target registry is not set up properly") {
		t.Error("the refusal does not name the target registry")
	}
}

func TestSyncRefusesToWriteWhileLocked(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.auth.Lock()

	resp := h.post("/sync/settings", syncForm(nil))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /sync/settings = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/unlock" {
		t.Errorf("Location = %q, want /unlock", got)
	}

	stored, err := h.auth.ConfiguredCredentials(t.Context())
	if err != nil {
		t.Fatalf("ConfiguredCredentials: %v", err)
	}
	if len(stored) != 0 {
		t.Error("a credential was written while the panel was locked")
	}
}

func TestSyncDispatchRefusesBeforeItCallsAnything(t *testing.T) {
	h := newHarness(t)
	h.setup()

	// Nothing configured at all, so the spec does not validate and no request
	// to GitHub is made.
	resp := h.post("/sync/dispatch", url.Values{"images": {"alpine:3.21"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /sync/dispatch = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(h.body(resp), "not set up properly") {
		t.Error("the refusal does not say the repository is the problem")
	}
}

func TestSyncDispatchNeedsSomethingToCopy(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.body(h.post("/sync/settings", syncForm(nil)))

	// Blank lines are not addresses, and an empty list never reaches the API.
	resp := h.post("/sync/dispatch", url.Values{"images": {"\n   \n"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /sync/dispatch = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(h.body(resp), "nothing to copy") {
		t.Error("the refusal does not say there was nothing to copy")
	}
}

func TestSyncRunsExplainAMissingToken(t *testing.T) {
	h := newHarness(t)
	h.setup()

	// Without a token there is no client, so this answers from what it knows
	// rather than by making a call that could only fail.
	body := h.body(h.get("/sync?runs=1"))

	if !strings.Contains(body, "No GitHub token is usable") {
		t.Errorf("the run list does not explain the missing token:\n%s", body)
	}
}

func TestSyncRunListIsOptIn(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.body(h.post("/sync/settings", syncForm(nil)))

	// A page load that was not asked for runs must not carry a run list, and
	// must not carry its error either.
	body := h.body(h.get("/sync"))

	if strings.Contains(body, "No GitHub token is usable") {
		t.Error("the runs error rendered on a page load that did not ask for runs")
	}
	if !strings.Contains(body, "Load recent runs") {
		t.Error("the page does not offer to load the runs")
	}
}

func TestSyncStatesAreLabelledAndColoured(t *testing.T) {
	tr := testTranslator(t)

	cases := map[ghsync.State]struct {
		label string
		class string
	}{
		ghsync.StateQueued:    {"queued", "pill-muted"},
		ghsync.StateRunning:   {"running", "pill-info"},
		ghsync.StateSucceeded: {"succeeded", "pill-ok"},
		ghsync.StateFailed:    {"failed", "pill-bad"},
		ghsync.StateCancelled: {"cancelled", "pill-warn"},
		ghsync.StateUnknown:   {"unknown", "pill-muted"},
	}

	for state, want := range cases {
		if got := syncStateLabel(state, tr); got != want.label {
			t.Errorf("syncStateLabel(%q) = %q, want %q", state, got, want.label)
		}
		if got := syncStateClass(state); got != want.class {
			t.Errorf("syncStateClass(%q) = %q, want %q", state, got, want.class)
		}
	}

	// A state this build has never heard of must render as itself rather than
	// as a blank pill.
	if got := syncStateLabel("startup_failure", tr); got != "startup_failure" {
		t.Errorf("an unknown state = %q, want it echoed back", got)
	}
	if got := syncStateClass("startup_failure"); got != "pill-muted" {
		t.Errorf("an unknown state = %q, want the muted class", got)
	}
}
func TestSplitImagesIgnoresBlankLines(t *testing.T) {
	got := splitImages("alpine:3.21\n\n  ghcr.io/owner/image:latest  \r\n")

	if len(got) != 2 {
		t.Fatalf("splitImages = %v, want two addresses", got)
	}
	if got[0] != "alpine:3.21" || got[1] != "ghcr.io/owner/image:latest" {
		t.Errorf("splitImages = %v", got)
	}
}

func TestFormatWhenAndDurationStayQuietAboutUnknowns(t *testing.T) {
	if got := formatWhen(time.Time{}); got != "" {
		t.Errorf("formatWhen(zero) = %q, want empty", got)
	}
	if got := formatDuration(0); got != "" {
		t.Errorf("formatDuration(0) = %q, want empty", got)
	}
	if got := formatDuration(90 * time.Second); got != "1m30s" {
		t.Errorf("formatDuration(90s) = %q", got)
	}
}

// fakeGitHub answers the three endpoints the relocation page uses.
//
// The panel talks to a real ghsync.Client pointed at this, not to a stand-in
// for one: building the request path, decoding the payload and classifying the
// status are the parts worth covering, and an interface would replace exactly
// those with whatever the fake decided to return.
type fakeGitHub struct {
	t *testing.T

	runsStatus int
	runsBody   string
	jobsStatus int
	jobsBody   string

	// dispatchStatus is what the dispatches endpoint answers with. Zero means
	// 204, which is what GitHub actually returns: the request is accepted and
	// nothing else is said about it.
	dispatchStatus int

	mu             sync.Mutex
	runsCalls      int
	jobsPaths      []string
	dispatchPaths  []string
	dispatchBodies []string
}

// maxDispatchBody caps what the fake reads back, so a runaway request cannot
// make a failing test allocate without bound.
const maxDispatchBody = 1 << 16

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.HasSuffix(r.URL.Path, "/dispatches"):
		// The body is kept verbatim: what the panel sends is the whole
		// contract with the workflow, and it is not visible anywhere else.
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxDispatchBody))
		if err != nil {
			f.t.Errorf("read dispatch body: %v", err)
		}
		f.dispatchPaths = append(f.dispatchPaths, r.URL.Path)
		f.dispatchBodies = append(f.dispatchBodies, string(raw))

		status := f.dispatchStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		f.reply(w, status, "")
	case strings.HasSuffix(r.URL.Path, "/jobs"):
		f.jobsPaths = append(f.jobsPaths, r.URL.Path)
		f.reply(w, f.jobsStatus, f.jobsBody)
	case strings.HasSuffix(r.URL.Path, "/runs"):
		f.runsCalls++
		f.reply(w, f.runsStatus, f.runsBody)
	default:
		f.t.Errorf("the panel asked for %s, which this fake does not serve", r.URL.Path)
		f.reply(w, http.StatusNotFound, `{"message":"not found"}`)
	}
}

func (f *fakeGitHub) reply(w http.ResponseWriter, status int, body string) {
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, body); err != nil {
		f.t.Errorf("write reply: %v", err)
	}
}

// calls reports how many times each endpoint was hit, under the same lock the
// handler uses so -race has nothing to complain about.
func (f *fakeGitHub) calls() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runsCalls, append([]string(nil), f.jobsPaths...)
}

// dispatches reports the dispatch calls seen so far, path and body.
func (f *fakeGitHub) dispatches() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dispatchPaths...), append([]string(nil), f.dispatchBodies...)
}

// twoRuns is a listing with one successful and one failed run.
const twoRuns = `{"workflow_runs":[
  {"id":42,"status":"completed","conclusion":"success","event":"workflow_dispatch",
   "name":"Relay alpine","html_url":"https://github.com/DC1024/image-relay/actions/runs/42",
   "head_branch":"main","created_at":"2026-10-24T10:00:00Z"},
  {"id":41,"status":"completed","conclusion":"failure","event":"workflow_dispatch",
   "name":"Relay busybox","html_url":"https://github.com/DC1024/image-relay/actions/runs/41",
   "head_branch":"main","created_at":"2026-10-23T09:00:00Z"}]}`

// newRelayHarness wires the panel to a fake GitHub and stores the settings the
// relocation page needs, so the run list has somewhere to read from.
func newRelayHarness(t *testing.T, api *fakeGitHub) *harness {
	t.Helper()

	api.t = t
	gh := httptest.NewServer(api)
	t.Cleanup(gh.Close)

	h := newHarness(t, func(o *Options) {
		o.GHClient = func(token string) (*ghsync.Client, error) {
			return ghsync.NewClient(token, ghsync.WithBaseURL(gh.URL))
		}
	})

	if resp := h.setup(); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303; body: %s", resp.StatusCode, h.body(resp))
	}
	h.body(h.post("/sync/settings", syncForm(nil)))
	return h
}

func TestSyncRunsAreListed(t *testing.T) {
	api := &fakeGitHub{runsBody: twoRuns}
	h := newRelayHarness(t, api)

	body := h.body(h.get("/sync?runs=1"))

	for _, want := range []string{"#42", "#41", "actions/runs/42", "main"} {
		if !strings.Contains(body, want) {
			t.Errorf("the run list is missing %q:\n%s", want, body)
		}
	}
	if runs, jobs := api.calls(); runs != 1 || len(jobs) != 0 {
		t.Errorf("calls = %d to runs, %d to jobs; want 1 and 0", runs, len(jobs))
	}
}

func TestSyncRunsSayWhenThereAreNone(t *testing.T) {
	api := &fakeGitHub{runsBody: `{"workflow_runs":[]}`}
	h := newRelayHarness(t, api)

	body := h.body(h.get("/sync?runs=1"))

	// A workflow that has never run is a normal state, not an error, and the
	// page has to say which of the two it is looking at.
	if !strings.Contains(body, "No runs yet") {
		t.Errorf("an empty listing is not explained:\n%s", body)
	}
}

func TestSyncJobsAreListedForTheSelectedRun(t *testing.T) {
	api := &fakeGitHub{
		runsBody: twoRuns,
		jobsBody: `{"jobs":[{"id":7,"name":"relay","status":"completed","conclusion":"success",
		  "html_url":"https://github.com/DC1024/image-relay/actions/runs/42/job/7",
		  "started_at":"2026-10-24T10:00:00Z","completed_at":"2026-10-24T10:01:30Z"}]}`,
	}
	h := newRelayHarness(t, api)

	body := h.body(h.get("/sync?runs=1&run=42"))

	if !strings.Contains(body, "Jobs in run") {
		t.Errorf("the selected run has no job section:\n%s", body)
	}
	if !strings.Contains(body, "relay") || !strings.Contains(body, "1m30s") {
		t.Errorf("the job row is missing its name or duration:\n%s", body)
	}

	_, jobs := api.calls()
	if len(jobs) != 1 || !strings.Contains(jobs[0], "/actions/runs/42/jobs") {
		t.Errorf("jobs requests = %v, want one for run 42", jobs)
	}
}

func TestSyncJobsAreNotFetchedForARunTheListDoesNotContain(t *testing.T) {
	api := &fakeGitHub{runsBody: twoRuns}
	h := newRelayHarness(t, api)

	// 999 was never listed. A hand-typed id must not become an arbitrary API
	// call made on the visitor's behalf.
	body := h.body(h.get("/sync?runs=1&run=999"))

	if strings.Contains(body, "Jobs in run") {
		t.Error("the page claims to show jobs for a run it never listed")
	}
	if _, jobs := api.calls(); len(jobs) != 0 {
		t.Errorf("a run id the listing did not contain reached the API: %v", jobs)
	}
}

func TestSyncRunsExplainAFailedListing(t *testing.T) {
	api := &fakeGitHub{runsStatus: http.StatusInternalServerError, runsBody: `{"message":"boom"}`}
	h := newRelayHarness(t, api)

	body := h.body(h.get("/sync?runs=1"))

	if !strings.Contains(body, "GitHub could not answer") {
		t.Errorf("a 500 from the listing is not explained:\n%s", body)
	}
	if _, jobs := api.calls(); len(jobs) != 0 {
		t.Errorf("jobs were fetched despite the listing failing: %v", jobs)
	}
}

func TestSyncJobsExplainAFailedListing(t *testing.T) {
	api := &fakeGitHub{
		runsBody:   twoRuns,
		jobsStatus: http.StatusBadGateway,
		jobsBody:   `{"message":"bad gateway"}`,
	}
	h := newRelayHarness(t, api)

	body := h.body(h.get("/sync?runs=1&run=42"))

	if !strings.Contains(body, "GitHub could not answer") {
		t.Errorf("a failed job listing is not explained:\n%s", body)
	}
	if !strings.Contains(body, "#42") {
		t.Error("the run list disappeared when only the job listing failed")
	}
}

func TestSyncRunsExplainARejectedToken(t *testing.T) {
	api := &fakeGitHub{runsStatus: http.StatusUnauthorized, runsBody: `{"message":"Bad credentials"}`}
	h := newRelayHarness(t, api)

	body := h.body(h.get("/sync?runs=1"))

	// A revoked token is a configuration state, not a bug, so the page says
	// what to do about it rather than showing the status text.
	if !strings.Contains(body, "No GitHub token is usable") {
		t.Errorf("a 401 from the listing is not explained:\n%s", body)
	}
}

func TestSyncDispatchStartsARun(t *testing.T) {
	api := &fakeGitHub{runsBody: twoRuns}
	h := newRelayHarness(t, api)

	resp := h.post("/sync/dispatch", url.Values{"images": {"alpine:3.21\nbusybox:1.36"}})
	body := h.body(resp)

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dispatch = %d, want 303; body: %s", resp.StatusCode, body)
	}

	// The confirmation is a flash on the next page rather than a message on
	// this response: a dispatch only queues a run, so there is nothing on it
	// to render, and the run list is what answers "did it work".
	want := "/sync?runs=1&flash=sync.dispatched"
	if got := resp.Header.Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	paths, bodies := api.dispatches()
	if len(paths) != 1 {
		t.Fatalf("dispatch calls = %v, want exactly one", paths)
	}
	if want := "/repos/DC1024/image-relay/actions/workflows/mirrorpilot-relay.yml/dispatches"; paths[0] != want {
		t.Errorf("dispatch path = %q, want %q", paths[0], want)
	}

	// The body is the whole contract with the workflow file, and nothing else
	// in the panel can show what was actually sent.
	for _, want := range []string{
		`"target":"registry.cn-hangzhou.aliyuncs.com/dc-images"`,
		`"images":"alpine:3.21\nbusybox:1.36"`,
		`"ref":"main"`,
	} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("the dispatch body is missing %s:\n%s", want, bodies[0])
		}
	}

	if page := h.body(h.get("/sync?runs=1&flash=sync.dispatched")); !strings.Contains(page, "Run dispatched") {
		t.Errorf("the flash never reaches the page it redirects to:\n%s", page)
	}
}

func TestSyncDispatchKeepsTheImagesWhenGitHubRefuses(t *testing.T) {
	api := &fakeGitHub{
		runsBody:       twoRuns,
		dispatchStatus: http.StatusNotFound,
	}
	h := newRelayHarness(t, api)

	resp := h.post("/sync/dispatch", url.Values{"images": {"alpine:3.21\nbusybox:1.36"}})
	body := h.body(resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a refused dispatch = %d, want 400; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "GitHub does not know that repository or workflow") {
		t.Errorf("a 404 from the dispatch is not explained:\n%s", body)
	}

	// GitHub's own wording is the diagnosis ("Workflow does not have
	// 'workflow_dispatch' trigger"), so it has to survive to the page.
	if !strings.Contains(body, "not found") {
		t.Errorf("GitHub's message was dropped:\n%s", body)
	}

	// The pasted list comes back in the box. Losing it would mean retyping
	// every address to fix a mistake made in the settings.
	if !strings.Contains(body, "alpine:3.21") || !strings.Contains(body, "busybox:1.36") {
		t.Errorf("the images were dropped from the form:\n%s", body)
	}
}

func TestSyncDispatchRefusesWithoutATarget(t *testing.T) {
	api := &fakeGitHub{runsBody: twoRuns}
	h := newRelayHarness(t, api)

	// The pair can be cleared from the settings page, which is a legitimate
	// state: the repository is set but there is nowhere to copy to.
	h.body(h.post("/sync/settings", syncForm(map[string]string{
		"registry":  "",
		"namespace": "",
	})))

	resp := h.post("/sync/dispatch", url.Values{"images": {"alpine:3.21"}})
	body := h.body(resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dispatch without a target = %d, want 400; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "The target registry is not set up properly") {
		t.Errorf("the refusal does not name the target registry:\n%s", body)
	}
	if paths, _ := api.dispatches(); len(paths) != 0 {
		t.Errorf("a dispatch with no target reached the API: %v", paths)
	}
}

func TestSyncDispatchNeedsAToken(t *testing.T) {
	api := &fakeGitHub{runsBody: twoRuns}
	h := newRelayHarness(t, api)

	// Repository and target are set, the token has been removed. The dispatch
	// cannot be made, and a redirect would claim one was.
	h.body(h.post("/sync/settings", syncForm(map[string]string{"clear_github_token": "1"})))

	resp := h.post("/sync/dispatch", url.Values{"images": {"alpine:3.21"}})
	body := h.body(resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("dispatch without a token = %d, want 400; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "No GitHub token is usable") {
		t.Errorf("the refusal does not explain the missing token:\n%s", body)
	}
	if paths, _ := api.dispatches(); len(paths) != 0 {
		t.Errorf("a dispatch with no token reached the API: %v", paths)
	}
}

func TestSyncDispatchDoesNotRunWhileLocked(t *testing.T) {
	api := &fakeGitHub{runsBody: twoRuns}
	h := newRelayHarness(t, api)
	h.auth.Lock()

	resp := h.post("/sync/dispatch", url.Values{"images": {"alpine:3.21"}})
	h.body(resp)

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dispatch while locked = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/unlock" {
		t.Errorf("Location = %q, want /unlock", got)
	}
	if paths, _ := api.dispatches(); len(paths) != 0 {
		t.Errorf("a locked panel dispatched anyway: %v", paths)
	}
}
