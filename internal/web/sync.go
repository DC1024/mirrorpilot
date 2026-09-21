package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DC1024/mirrorpilot/internal/auth"
	"github.com/DC1024/mirrorpilot/internal/ghsync"
	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// syncStateKeys names the translation for each state a run or job can be in.
var syncStateKeys = map[ghsync.State]string{
	ghsync.StateQueued:    "sync.state.queued",
	ghsync.StateRunning:   "sync.state.running",
	ghsync.StateSucceeded: "sync.state.succeeded",
	ghsync.StateFailed:    "sync.state.failed",
	ghsync.StateCancelled: "sync.state.cancelled",
	ghsync.StateUnknown:   "sync.state.unknown",
}

// syncStateClasses colour a state.
//
// Running and queued share the muted/informational end deliberately: neither
// is an outcome, and colouring "queued" green would read as a result that has
// not happened yet.
var syncStateClasses = map[ghsync.State]string{
	ghsync.StateQueued:    "pill-muted",
	ghsync.StateRunning:   "pill-info",
	ghsync.StateSucceeded: "pill-ok",
	ghsync.StateFailed:    "pill-bad",
	ghsync.StateCancelled: "pill-warn",
	ghsync.StateUnknown:   "pill-muted",
}

// runListSize is how many runs the page asks for.
//
// More than shown, because a dispatch that has already been superseded by two
// other runs is exactly when someone wants to look further back.
const runListSize = 10

// syncRunView is one workflow run as the page shows it.
type syncRunView struct {
	ID    int64
	Label string
	Class string

	Title  string
	Branch string
	URL    string

	// Created is the timestamp, already rendered.
	Created string
}

// syncJobView is one job inside a run.
type syncJobView struct {
	Name     string
	Label    string
	Class    string
	URL      string
	Duration string
}

// syncPage backs the relocation page.
type syncPage struct {
	// Unlocked gates writing credentials: the sealer needs the master key,
	// which is not on disk.
	Unlocked bool

	WorkflowYAML     string
	WorkflowFileName string

	Spec   ghsync.Spec
	Target ghsync.Target

	// HasToken and HasPassword say which credential slots are populated.
	// Never the values: the page has no business rendering a secret it is
	// about to hand to someone's screen.
	HasToken    bool
	HasPassword bool

	// Configured reports whether a dispatch could be attempted at all, so the
	// button can explain itself instead of failing.
	Configured bool

	// Images is what was typed into the dispatch box, echoed back after a
	// rejection.
	Images string

	// Error and Detail describe a rejected submission.
	Error  string
	Detail string

	// RunsLoaded reports whether the run list was asked for and answered.
	RunsLoaded bool
	RunsError  string
	Runs       []syncRunView

	// Selected is the run whose jobs are shown, zero when none.
	Selected  int64
	Jobs      []syncJobView
	JobsError string
}

// handleSync shows the relocation setup and, on request, what GitHub did.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	page, err := s.loadSyncPage(r)
	if err != nil {
		s.fail(w, r, "web: load relocation page", err)
		return
	}

	s.renderSync(w, r, page, http.StatusOK)
}

// renderSync writes the relocation page.
func (s *Server) renderSync(w http.ResponseWriter, r *http.Request, page *syncPage, status int) {
	data := s.newPageData(r, stateFrom(r.Context()), "sync.title", "nav.sync")
	data.SyncPage = page

	s.render(w, r, "sync", status, data)
}

// loadSyncPage assembles the relocation page.
func (s *Server) loadSyncPage(r *http.Request) (*syncPage, error) {
	ctx := r.Context()
	st := stateFrom(ctx)

	spec, target, err := s.syncSettings(ctx)
	if err != nil {
		return nil, err
	}

	// Deliberately does not need the master key: rendering "configured" for a
	// locked panel is the honest answer, and asking the sealer would make the
	// page fail to render for a reason that has nothing to do with it.
	configured, err := s.auth.ConfiguredCredentials(ctx)
	if err != nil {
		return nil, err
	}

	page := &syncPage{
		Unlocked:         st.Unlocked,
		WorkflowYAML:     ghsync.WorkflowYAML,
		WorkflowFileName: ghsync.DefaultWorkflowFile,
		Spec:             spec,
		Target:           target,
		HasToken:         configured[store.CredentialGitHubToken],
		HasPassword:      configured[store.CredentialACRPassword],
		Images:           r.URL.Query().Get("images"),
	}

	page.Configured = st.Unlocked && page.HasToken &&
		spec.Validate() == nil && target.Validate() == nil

	// The run list is opt-in. Loading it on every visit would put a call to
	// GitHub in front of every page render — a network round trip the visitor
	// did not ask for, on a page whose main job is showing them a document to
	// copy.
	if r.URL.Query().Get("runs") == "" {
		return page, nil
	}

	page.RunsLoaded = true
	s.loadRuns(ctx, page, parseRunID(r.URL.Query().Get("run")))
	return page, nil
}

// syncSettings reads the stored relocation settings.
func (s *Server) syncSettings(ctx context.Context) (ghsync.Spec, ghsync.Target, error) {
	owner, err := s.store.SettingOrDefault(ctx, store.SettingSyncOwner, "")
	if err != nil {
		return ghsync.Spec{}, ghsync.Target{}, err
	}
	repo, err := s.store.SettingOrDefault(ctx, store.SettingSyncRepo, "")
	if err != nil {
		return ghsync.Spec{}, ghsync.Target{}, err
	}
	workflow, err := s.store.SettingOrDefault(ctx, store.SettingSyncWorkflow, ghsync.DefaultWorkflowFile)
	if err != nil {
		return ghsync.Spec{}, ghsync.Target{}, err
	}
	ref, err := s.store.SettingOrDefault(ctx, store.SettingSyncRef, "")
	if err != nil {
		return ghsync.Spec{}, ghsync.Target{}, err
	}
	registry, err := s.store.SettingOrDefault(ctx, store.SettingACRRegistry, "")
	if err != nil {
		return ghsync.Spec{}, ghsync.Target{}, err
	}
	namespace, err := s.store.SettingOrDefault(ctx, store.SettingACRNamespace, "")
	if err != nil {
		return ghsync.Spec{}, ghsync.Target{}, err
	}

	return ghsync.Spec{Owner: owner, Repo: repo, Workflow: workflow, Ref: ref},
		ghsync.Target{Registry: registry, Namespace: namespace},
		nil
}

// syncClient builds a GitHub client from the stored token.
func (s *Server) syncClient(ctx context.Context) (*ghsync.Client, error) {
	token, err := s.auth.Credential(ctx, store.CredentialGitHubToken)
	if err != nil {
		return nil, err
	}
	return ghsync.NewClient(string(token))
}

// loadRuns fills in the recent runs and, when asked, one run's jobs.
func (s *Server) loadRuns(ctx context.Context, page *syncPage, selected int64) {
	tr := s.i18n.Translator(stateFrom(ctx).Locale)

	client, err := s.syncClient(ctx)
	if err != nil {
		page.RunsError = tr.T(syncMessageKey(err))
		return
	}

	runs, err := client.Runs(ctx, page.Spec, runListSize)
	if err != nil {
		page.RunsError = s.syncFailure(ctx, "list workflow runs", err, tr)
		return
	}

	page.Runs = make([]syncRunView, 0, len(runs))
	for _, run := range runs {
		page.Runs = append(page.Runs, newSyncRunView(run, tr))
	}

	if len(page.Runs) == 0 {
		page.RunsError = tr.T("sync.error.no_runs")
		return
	}

	// Only a run the listing actually returned, so a hand-typed id cannot make
	// this an arbitrary API call on the visitor's behalf.
	if selected <= 0 || !page.hasRun(selected) {
		return
	}

	page.Selected = selected

	jobs, err := client.Jobs(ctx, page.Spec, selected)
	if err != nil {
		page.JobsError = s.syncFailure(ctx, "list workflow jobs", err, tr)
		return
	}

	page.Jobs = make([]syncJobView, 0, len(jobs))
	for _, job := range jobs {
		page.Jobs = append(page.Jobs, newSyncJobView(job, tr))
	}
}

// hasRun reports whether an id names a run the page is already showing.
func (p *syncPage) hasRun(id int64) bool {
	for _, run := range p.Runs {
		if run.ID == id {
			return true
		}
	}
	return false
}

// parseRunID reads a run id from the query string, returning zero for anything
// that is not one.
func parseRunID(raw string) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// handleSyncSettings stores the repository, target and credentials.
func (s *Server) handleSyncSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := stateFrom(ctx)

	// Credentials are sealed with the master key, so this page is meaningless
	// while locked. Sending the visitor to unlock is what they would do next
	// anyway.
	if !st.Unlocked {
		redirect(w, r, "/unlock")
		return
	}

	spec := ghsync.Spec{
		Owner:    strings.TrimSpace(r.PostFormValue("owner")),
		Repo:     strings.TrimSpace(r.PostFormValue("repo")),
		Workflow: strings.TrimSpace(r.PostFormValue("workflow")),
		Ref:      strings.TrimSpace(r.PostFormValue("ref")),
	}
	if spec.Workflow == "" {
		spec.Workflow = ghsync.DefaultWorkflowFile
	}

	target := ghsync.Target{
		Registry:  strings.TrimSpace(r.PostFormValue("registry")),
		Namespace: strings.TrimSpace(r.PostFormValue("namespace")),
	}

	if key, detail := validateSyncForms(spec, target); key != "" {
		// Re-rendered with what was typed rather than with what is stored, so
		// a rejected submission costs a correction instead of the whole form.
		page, err := s.loadSyncPage(r)
		if err != nil {
			s.fail(w, r, "web: load relocation page", err)
			return
		}

		page.Spec = spec
		page.Target = target
		page.Error = s.i18n.Translator(st.Locale).T(key)
		page.Detail = detail

		s.renderSync(w, r, page, http.StatusBadRequest)
		return
	}

	if err := s.saveSyncForms(ctx, spec, target); err != nil {
		s.fail(w, r, "web: save relocation settings", err)
		return
	}

	if err := s.saveSyncCredentials(ctx, r); err != nil {
		s.fail(w, r, "web: save relocation credentials", err)
		return
	}

	redirect(w, r, "/sync?flash=sync.saved")
}

// validateSyncForms reports whether the two forms can be stored.
//
// A blank pair means "not configured", which is a legitimate state on a fresh
// install and clears the pair. One field without its partner would be stored as
// a spec that fails validation on every later page load, with an error nobody
// typed on that visit — so it is refused here, where the mistake was made.
func validateSyncForms(spec ghsync.Spec, target ghsync.Target) (key, detail string) {
	if (spec.Owner == "") != (spec.Repo == "") {
		return "sync.error.spec", ""
	}
	if spec.Owner != "" {
		if err := spec.Validate(); err != nil {
			return "sync.error.spec", err.Error()
		}
	}

	if (target.Registry == "") != (target.Namespace == "") {
		return "sync.error.target", ""
	}
	if target.Registry != "" {
		if err := target.Validate(); err != nil {
			return "sync.error.target", err.Error()
		}
	}
	return "", ""
}

// saveSyncForms writes the non-secret half.
func (s *Server) saveSyncForms(ctx context.Context, spec ghsync.Spec, target ghsync.Target) error {
	pairs := []struct{ key, value string }{
		{store.SettingSyncOwner, spec.Owner},
		{store.SettingSyncRepo, spec.Repo},
		{store.SettingSyncWorkflow, spec.Workflow},
		{store.SettingSyncRef, spec.Ref},
		{store.SettingACRRegistry, target.Registry},
		{store.SettingACRNamespace, target.Namespace},
	}

	for _, pair := range pairs {
		if err := s.storeSettingOrClear(ctx, pair.key, pair.value); err != nil {
			return err
		}
	}
	return nil
}

// saveSyncCredentials writes the secret half.
//
// An empty field leaves the stored value alone rather than clearing it: the
// form cannot show what is already there, so blank has to mean "unchanged" or
// every save of the unrelated fields would wipe the token. Removing one is
// therefore its own control, with its own explicit intent.
func (s *Server) saveSyncCredentials(ctx context.Context, r *http.Request) error {
	token := strings.TrimSpace(r.PostFormValue("github_token"))
	if r.PostFormValue("clear_github_token") != "" {
		if err := s.auth.DeleteCredential(ctx, store.CredentialGitHubToken); err != nil {
			return err
		}
	} else if token != "" {
		if err := s.auth.SaveCredential(ctx, store.CredentialGitHubToken, []byte(token)); err != nil {
			return err
		}
	}

	password := strings.TrimSpace(r.PostFormValue("acr_password"))
	if r.PostFormValue("clear_acr_password") != "" {
		if err := s.auth.DeleteCredential(ctx, store.CredentialACRPassword); err != nil {
			return err
		}
	} else if password != "" {
		if err := s.auth.SaveCredential(ctx, store.CredentialACRPassword, []byte(password)); err != nil {
			return err
		}
	}

	return nil
}

// handleSyncDispatch asks GitHub to relocate a list of images.
func (s *Server) handleSyncDispatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if !stateFrom(ctx).Unlocked {
		redirect(w, r, "/unlock")
		return
	}

	images := splitImages(r.PostFormValue("images"))

	spec, target, err := s.syncSettings(ctx)
	if err != nil {
		s.fail(w, r, "web: read relocation settings", err)
		return
	}
	if err := spec.Validate(); err != nil {
		s.renderSyncProblem(w, r, "sync.error.spec", "", strings.Join(images, "\n"))
		return
	}
	if err := target.Validate(); err != nil {
		s.renderSyncProblem(w, r, "sync.error.target", "", strings.Join(images, "\n"))
		return
	}

	inputs := ghsync.Inputs{Target: target.Prefix(), Images: images}
	if err := inputs.Validate(); err != nil {
		s.renderSyncProblem(w, r, "sync.error.images", "", strings.Join(images, "\n"))
		return
	}

	client, err := s.syncClient(ctx)
	if err != nil {
		s.renderSyncProblem(w, r, syncMessageKey(err), "", strings.Join(images, "\n"))
		return
	}

	if err := client.Dispatch(ctx, spec, inputs); err != nil {
		key := syncMessageKey(err)
		s.renderSyncProblem(w, r, key, syncDetail(err, key), strings.Join(images, "\n"))
		return
	}

	s.log.InfoContext(ctx, "web: dispatched a relocation run",
		"repo", spec.Owner+"/"+spec.Repo, "images", len(images))

	redirect(w, r, "/sync?runs=1&flash=sync.dispatched")
}

// renderSyncProblem re-renders the page with a rejection on it.
func (s *Server) renderSyncProblem(w http.ResponseWriter, r *http.Request, key, detail, images string) {
	page, err := s.loadSyncPage(r)
	if err != nil {
		s.fail(w, r, "web: load relocation page", err)
		return
	}

	page.Error = s.i18n.Translator(stateFrom(r.Context()).Locale).T(key)
	page.Detail = detail
	page.Images = images

	// The runs list is not reloaded: the submission was refused before any
	// call was made, so there is nothing new to show and a round trip would
	// only delay the explanation.
	page.RunsLoaded = false
	page.Runs = nil
	page.RunsError = ""
	page.Selected = 0
	page.Jobs = nil

	s.renderSync(w, r, page, http.StatusBadRequest)
}

// splitImages reads the dispatch box into a list of addresses.
//
// Newline separated, one per line, because that is what a person can paste out
// of a README. Blank lines are dropped rather than refused: a trailing newline
// is not a mistake worth blocking on.
func splitImages(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// syncMessageKey maps a failed GitHub call onto something the page can say.
//
// Written out here rather than built from an error's text, so the catalogue
// check can see which keys this file consumes.
func syncMessageKey(err error) string {
	switch {
	// store.ErrNotFound is "no token is stored yet", which is the same
	// instruction to the reader as a token GitHub refused: put a good one in.
	case errors.Is(err, ghsync.ErrUnauthorized), errors.Is(err, store.ErrNotFound):
		return "sync.error.token"
	case errors.Is(err, ghsync.ErrForbidden):
		return "sync.error.forbidden"
	case errors.Is(err, ghsync.ErrNotFound):
		return "sync.error.not_found"
	case errors.Is(err, ghsync.ErrRateLimited):
		return "sync.error.rate_limited"
	case errors.Is(err, ghsync.ErrRejected):
		return "sync.error.rejected"
	case errors.Is(err, ghsync.ErrInvalid):
		return "sync.error.spec"
	case errors.Is(err, auth.ErrLocked):
		return "error.locked"
	default:
		return "error.internal"
	}
}

// syncDetail returns the validator's or GitHub's own wording, for the keys
// where it says something the translation cannot.
//
// GitHub's messages are the whole diagnosis ("Workflow does not have
// 'workflow_dispatch' trigger"), so they are shown as-is. The keys that carry a
// fixed instruction instead get no detail.
func syncDetail(err error, key string) string {
	switch key {
	case "sync.error.rejected", "sync.error.not_found":
		return err.Error()
	default:
		return ""
	}
}

// syncFailure logs a failed call and returns the message to show.
func (s *Server) syncFailure(ctx context.Context, what string, err error, tr *i18n.Translator) string {
	key := syncMessageKey(err)

	// A missing token is a configuration state, not a bug, and logging it as
	// one would put noise in the log every time someone visits the page
	// before setting up.
	if key != "sync.error.token" {
		s.log.ErrorContext(ctx, "web: "+what, "err", err)
	}
	return tr.T(key)
}

// newSyncRunView renders one run.
func newSyncRunView(run ghsync.Run, tr *i18n.Translator) syncRunView {
	state := run.State()

	return syncRunView{
		ID:      run.ID,
		Label:   syncStateLabel(state, tr),
		Class:   syncStateClass(state),
		Title:   run.Title,
		Branch:  run.HeadBranch,
		URL:     run.URL,
		Created: formatWhen(run.CreatedAt),
	}
}

// newSyncJobView renders one job.
func newSyncJobView(job ghsync.Job, tr *i18n.Translator) syncJobView {
	state := job.State()

	return syncJobView{
		Name:     job.Name,
		Label:    syncStateLabel(state, tr),
		Class:    syncStateClass(state),
		URL:      job.URL,
		Duration: formatDuration(job.Duration()),
	}
}

// syncStateLabel translates a state, falling back to the raw value.
//
// The fallback is for a state written by a newer version and read by an older
// one. Showing "startup_failure" is ugly; showing a blank pill is a lie.
func syncStateLabel(state ghsync.State, tr *i18n.Translator) string {
	key, ok := syncStateKeys[state]
	if !ok {
		return string(state)
	}
	return tr.T(key)
}

func syncStateClass(state ghsync.State) string {
	if class, ok := syncStateClasses[state]; ok {
		return class
	}
	return "pill-muted"
}

// formatWhen renders a timestamp in the panel host's own timezone.
//
// Local rather than UTC because the reader is comparing against their own
// clock, and a run they triggered a moment ago should look like it.
func formatWhen(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

// formatDuration renders how long a job took, empty when it has not finished.
//
// A job that is still running has no duration, which is not the same as a
// duration of zero.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.Round(time.Second).String()
}
