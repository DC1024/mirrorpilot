package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/DC1024/mirrorpilot/internal/catalog"
	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/mirror"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// The label tables below name every value of a mirror enum, in one place.
//
// They are written out rather than assembled by concatenating a prefix onto
// each enum value. A built key is invisible to the catalogue check that proves
// every shipped string is actually shown, so the translations would quietly
// become dead weight — and a table is the one place a missing label is obvious
// on review.
var (
	providerKeys = map[mirror.Provider]string{
		mirror.ProviderOfficial:  "sources.provider.official",
		mirror.ProviderCloud:     "sources.provider.cloud",
		mirror.ProviderAcademic:  "sources.provider.academic",
		mirror.ProviderCompany:   "sources.provider.company",
		mirror.ProviderCommunity: "sources.provider.community",
		mirror.ProviderPersonal:  "sources.provider.personal",
	}

	trustKeys = map[mirror.Trust]string{
		mirror.TrustVerified: "sources.trust.verified",
		mirror.TrustKnown:    "sources.trust.known",
		mirror.TrustUnknown:  "sources.trust.unknown",
	}

	scopeKeys = map[mirror.Scope]string{
		mirror.ScopeDockerHub: "sources.scope.dockerhub",
		mirror.ScopeGHCR:      "sources.scope.ghcr",
		mirror.ScopeQuay:      "sources.scope.quay",
		mirror.ScopeGCR:       "sources.scope.gcr",
		mirror.ScopeK8s:       "sources.scope.k8s",
		mirror.ScopeMCR:       "sources.scope.mcr",
	}

	// trustClasses colour the trust badge.
	//
	// Deliberately not a green/amber/red verdict on quality: the grade records
	// how much is publicly known about the operator, and a mirror nobody
	// publishes anything about may still be the fastest one on the list.
	trustClasses = map[mirror.Trust]string{
		mirror.TrustVerified: "pill-ok",
		mirror.TrustKnown:    "pill-info",
		mirror.TrustUnknown:  "pill-warn",
	}
)

// tagView is one small labelled badge.
type tagView struct {
	// Label is the human-readable text, already translated when Key was set.
	Label string

	// Class is the extra CSS class, empty when the badge needs no colour.
	Class string
}

// choice is one option in a form select.
type choice struct {
	Value string
	Label string
}

// catalogSummary is the dashboard's overview of the catalogue.
type catalogSummary struct {
	Total   int
	Enabled int

	// Probed is how many mirrors have ever been measured, and LastProbe is
	// when the newest measurement was taken. A zero LastProbe means nothing
	// has been probed yet, which the template renders as "never".
	Probed    int
	LastProbe time.Time
}

// mirrorsPage backs the mirror management page.
type mirrorsPage struct {
	Mirrors []mirrorView
	Form    mirrorForm

	Scopes    []choice
	Providers []choice
	Trusts    []choice
}

// mirrorView is one row in the mirror list.
type mirrorView struct {
	ID       string
	Name     string
	URL      string
	Homepage string
	Note     string

	Scopes   []tagView
	Provider tagView
	Trust    tagView

	Builtin  bool
	Enabled  bool
	Insecure bool

	// Latest is the newest measurement, nil when this mirror has never been
	// probed. The template distinguishes the two: a mirror that is down is not
	// the same thing as one that has not been tried.
	Latest *probeRun
}

// mirrorForm is the add/edit form's state.
//
// It carries what the user typed back to the page after a rejected submission,
// so a mistake costs them a correction rather than the whole form.
type mirrorForm struct {
	ID       string
	Name     string
	URL      string
	Homepage string
	Note     string
	Scopes   []string
	Provider string
	Trust    string
	Insecure bool
	Enabled  bool

	// Editing marks an edit rather than an add, which decides the action the
	// form posts to and whether the id is editable.
	Editing bool

	// Error is the translated reason the submission was rejected.
	Error string

	// Detail is the untranslated reason, shown to the operator as-is.
	//
	// Validation messages name the offending field precisely. Restating every
	// rule in translated prose would be a second implementation of Validate to
	// keep in step with the first, and the second one would win.
	Detail string
}

// HasScope reports whether a scope checkbox should start checked.
func (f mirrorForm) HasScope(scope string) bool {
	for _, sc := range f.Scopes {
		if strings.EqualFold(strings.TrimSpace(sc), scope) {
			return true
		}
	}
	return false
}

// handleSources renders the mirror list and the form for adding one.
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	page, err := s.mirrorsPage(ctx, s.formFromQuery(r))
	if err != nil {
		s.fail(w, r, "web: load mirrors", err)
		return
	}

	s.renderMirrors(w, r, page, http.StatusOK)
}

// renderMirrors writes the mirror page with a page model already assembled.
func (s *Server) renderMirrors(w http.ResponseWriter, r *http.Request, page *mirrorsPage, status int) {
	data := s.newPageData(r, stateFrom(r.Context()), "sources.title", "nav.sources")
	data.MirrorsPage = page

	s.render(w, r, "sources", status, data)
}

// mirrorsPage assembles everything the mirror page shows.
func (s *Server) mirrorsPage(ctx context.Context, form mirrorForm) (*mirrorsPage, error) {
	records, err := s.store.ListSources(ctx)
	if err != nil {
		return nil, err
	}

	latest, err := s.store.LatestProbes(ctx)
	if err != nil {
		return nil, err
	}

	tr := s.i18n.Translator(stateFrom(ctx).Locale)

	views := make([]mirrorView, 0, len(records))
	for _, rec := range records {
		views = append(views, newMirrorView(rec, latest[rec.ID], tr))
	}

	return &mirrorsPage{
		Mirrors:   views,
		Form:      form,
		Scopes:    scopeChoices(tr),
		Providers: providerChoices(tr),
		Trusts:    trustChoices(tr),
	}, nil
}

// formFromQuery builds the form for a plain GET: blank, or holding the mirror
// named by ?edit=<id>.
//
// The blank form is the default rather than a separate page because adding a
// mirror is a two-field operation, and a dedicated page for it would be more
// navigation than form.
func (s *Server) formFromQuery(r *http.Request) mirrorForm {
	id := r.URL.Query().Get("edit")
	if id == "" {
		return blankMirrorForm()
	}

	rec, err := s.store.GetSource(r.Context(), id)
	if err != nil {
		// An edit target that has gone missing is not worth an error page:
		// falling back to the add form is what the visitor would do anyway.
		return blankMirrorForm()
	}

	return editMirrorForm(rec)
}

// blankMirrorForm is the add form before anything has been typed.
func blankMirrorForm() mirrorForm {
	return mirrorForm{
		Provider: string(mirror.ProviderCommunity),
		Trust:    string(mirror.TrustUnknown),

		// Docker Hub is what a registry mirror almost always means, and it is
		// the only upstream the probe can measure, so starting with it checked
		// is the answer that is right most of the time.
		Scopes: []string{string(mirror.ScopeDockerHub)},

		Enabled: true,
	}
}

// editMirrorForm prefills the form from a stored row.
func editMirrorForm(rec store.SourceRecord) mirrorForm {
	return mirrorForm{
		ID:       rec.ID,
		Name:     rec.Name,
		URL:      rec.URL,
		Homepage: rec.Homepage,
		Note:     rec.Note,
		Scopes:   append([]string(nil), rec.Scope...),
		Provider: rec.Provider,
		Trust:    rec.Trust,
		Insecure: rec.Insecure,
		Enabled:  rec.Enabled,
		Editing:  true,
	}
}

// formFromRequest reads a submitted mirror form.
func formFromRequest(r *http.Request, editing bool) mirrorForm {
	// Called explicitly rather than relying on PostFormValue having populated
	// r.PostForm already, because the checkbox slice is read from r.PostForm
	// directly and the order of those reads should not matter.
	_ = r.ParseForm()

	return mirrorForm{
		ID:       strings.TrimSpace(r.PostFormValue("id")),
		Name:     r.PostFormValue("name"),
		URL:      r.PostFormValue("url"),
		Homepage: r.PostFormValue("homepage"),
		Note:     r.PostFormValue("note"),
		Scopes:   r.PostForm["scope"],
		Provider: r.PostFormValue("provider"),
		Trust:    r.PostFormValue("trust"),
		Insecure: r.PostFormValue("insecure") != "",
		Enabled:  r.PostFormValue("enabled") != "",
		Editing:  editing,
	}
}

// toSource turns a submitted form into a domain source, or explains why not.
func (f mirrorForm) toSource() (mirror.Source, error) {
	scope := make([]mirror.Scope, 0, len(f.Scopes))
	for _, sc := range f.Scopes {
		scope = append(scope, mirror.Scope(sc))
	}

	src := mirror.Source{
		ID:       f.ID,
		Name:     f.Name,
		URL:      f.URL,
		Homepage: f.Homepage,
		Scope:    scope,
		Provider: mirror.Provider(f.Provider),
		Trust:    mirror.Trust(f.Trust),
		Note:     f.Note,
		Enabled:  f.Enabled,
		Insecure: f.Insecure,
	}.Normalize()

	if err := src.Validate(); err != nil {
		return mirror.Source{}, err
	}
	return src, nil
}

// handleSourceCreate adds a mirror the user supplies.
func (s *Server) handleSourceCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form := formFromRequest(r, false)

	src, err := form.toSource()
	if err != nil {
		s.renderMirrorFormError(w, r, form, err)
		return
	}

	if err := s.store.CreateSource(ctx, catalog.ToRecord(src)); err != nil {
		s.renderMirrorFormError(w, r, form, err)
		return
	}

	redirect(w, r, "/sources?flash=sources.created")
}

// handleSourceUpdate rewrites a mirror the user supplied.
func (s *Server) handleSourceUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rec, err := s.store.GetSource(ctx, r.PathValue("id"))
	if err != nil {
		s.redirectSourceError(w, r, err)
		return
	}

	// A built-in mirror may be switched off, and that is all. Rewriting the
	// address of an entry that still carries a "verified" badge would make the
	// badge meaningless, and the badge is the whole reason the trust grading
	// is worth showing.
	if rec.Builtin {
		redirect(w, r, "/sources?flash=sources.error.builtin_readonly")
		return
	}

	form := formFromRequest(r, true)

	// The id comes from the path, never from the body. It is the primary key
	// everywhere, so letting a form change it would orphan the user's
	// enabled/disabled choice and any probe history keyed on it — the row
	// would survive while everything referring to it stopped matching.
	form.ID = rec.ID

	src, err := form.toSource()
	if err != nil {
		s.renderMirrorFormError(w, r, form, err)
		return
	}

	if err := s.store.UpdateSource(ctx, catalog.ToRecord(src)); err != nil {
		s.renderMirrorFormError(w, r, form, err)
		return
	}

	redirect(w, r, "/sources?flash=sources.updated")
}

// handleSourceDelete removes a mirror the user supplied.
func (s *Server) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSource(r.Context(), r.PathValue("id")); err != nil {
		s.redirectSourceError(w, r, err)
		return
	}

	redirect(w, r, "/sources?flash=sources.deleted")
}

// handleSourceEnabled is the one change a built-in mirror allows.
//
// It takes the destination from the form so the same control works from the
// mirror list and from the speed test page.
func (s *Server) handleSourceEnabled(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.PostFormValue("next"), "/sources")

	if err := s.store.SetSourceEnabled(r.Context(), r.PathValue("id"), r.PostFormValue("enabled") == "true"); err != nil {
		s.redirectSourceError(w, r, err)
		return
	}

	redirect(w, r, next)
}

// renderMirrorFormError re-renders the mirror page with the submission intact.
//
// A 400 rather than a 200: the response is a rejection of the request, and a
// browser or proxy that caches it would serve the error back on the next visit.
func (s *Server) renderMirrorFormError(w http.ResponseWriter, r *http.Request, form mirrorForm, cause error) {
	form.Error, form.Detail = s.explainMirrorError(r, cause)

	page, err := s.mirrorsPage(r.Context(), form)
	if err != nil {
		s.fail(w, r, "web: load mirrors", err)
		return
	}

	s.renderMirrors(w, r, page, http.StatusBadRequest)
}

// explainMirrorError turns a store or validation failure into something the
// page can say.
func (s *Server) explainMirrorError(r *http.Request, cause error) (message, detail string) {
	tr := s.i18n.Translator(stateFrom(r.Context()).Locale)

	switch {
	case errors.Is(cause, mirror.ErrInvalid):
		// Show the validator's own wording. It names the offending field
		// precisely, and the panel has exactly one operator, who is the person
		// who needs that precision.
		return tr.T("sources.error.invalid"), cause.Error()

	case errors.Is(cause, store.ErrReadOnly):
		return tr.T("sources.error.builtin_readonly"), ""

	case errors.Is(cause, store.ErrNotFound):
		return tr.T("sources.error.not_found"), ""

	default:
		// A unique-key violation surfaces as a driver error rather than a
		// typed one, so the only way to name it is to look for the shape of
		// the message. Anything else is ours and belongs in the log.
		if isUniqueViolation(cause) {
			return tr.T("sources.error.duplicate_id"), ""
		}

		s.log.ErrorContext(r.Context(), "web: save mirror", "err", cause)
		return tr.T("error.internal"), ""
	}
}

// redirectSourceError sends the visitor back to the list with a flash naming
// what went wrong, rather than an error page.
//
// These are all cases the user can fix by looking at the list they came from:
// a row that is gone, or one that refuses to change.
func (s *Server) redirectSourceError(w http.ResponseWriter, r *http.Request, cause error) {
	tr := s.i18n.Translator(stateFrom(r.Context()).Locale)

	key := "sources.error.not_found"
	if errors.Is(cause, store.ErrReadOnly) {
		key = "sources.error.builtin_readonly"
	}

	// Logged even though it is expected: a user repeatedly hitting "read-only"
	// is a sign the UI is offering a control it should not.
	s.log.InfoContext(r.Context(), "web: mirror change refused",
		"err", cause, "message", tr.T(key))

	redirect(w, r, "/sources?flash="+key)
}

// isUniqueViolation reports whether an error is a primary-key collision.
//
// Matching on a driver message is unpleasant, but database/sql offers no typed
// alternative for a constraint violation and the modernc SQLite driver does not
// export one. The pattern is narrow enough that a false positive would need an
// error message to contain this exact phrase for another reason.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "UNIQUE CONSTRAINT FAILED") || strings.Contains(msg, "PRIMARY KEY")
}

// newMirrorView renders one stored row for the list.
func newMirrorView(rec store.SourceRecord, latest store.ProbeRecord, tr *i18n.Translator) mirrorView {
	scopes := make([]tagView, 0, len(rec.Scope))
	for _, sc := range rec.Scope {
		scopes = append(scopes, newScopeTag(sc, tr))
	}

	view := mirrorView{
		ID:       rec.ID,
		Name:     rec.Name,
		URL:      rec.URL,
		Homepage: rec.Homepage,
		Note:     rec.Note,
		Scopes:   scopes,
		Provider: newProviderTag(rec.Provider, tr),
		Trust:    newTrustTag(rec.Trust, tr),
		Builtin:  rec.Builtin,
		Enabled:  rec.Enabled,
		Insecure: rec.Insecure,
	}

	// A zero StartedAt means no row was found, which is how a mirror that has
	// never been probed stays distinguishable from one that failed.
	if !latest.StartedAt.IsZero() {
		run := newProbeRun(latest, tr)
		view.Latest = &run
	}

	return view
}

// newScopeTag labels one upstream scope.
func newScopeTag(value string, tr *i18n.Translator) tagView {
	key, ok := scopeKeys[mirror.Scope(value)]
	if !ok {
		// Reachable only from a hand-edited database: the form offers nothing
		// else. Showing the raw value is more honest than inventing a label.
		return tagView{Label: value}
	}
	return tagView{Label: tr.T(key)}
}

// newProviderTag labels the organisation that runs a mirror.
func newProviderTag(value string, tr *i18n.Translator) tagView {
	key, ok := providerKeys[mirror.Provider(value)]
	if !ok {
		return tagView{Label: value}
	}
	return tagView{Label: tr.T(key)}
}

// newTrustTag labels how much is publicly known about the operator.
func newTrustTag(value string, tr *i18n.Translator) tagView {
	key, ok := trustKeys[mirror.Trust(value)]
	if !ok {
		return tagView{Label: value}
	}
	return tagView{Label: tr.T(key), Class: trustClasses[mirror.Trust(value)]}
}

// scopeChoices, providerChoices and trustChoices build the form's selects.
//
// The order comes from the enum's own list, so the form does not reshuffle
// between releases and muscle memory keeps working.
func scopeChoices(tr *i18n.Translator) []choice {
	scopes := mirror.Scopes()

	out := make([]choice, 0, len(scopes))
	for _, sc := range scopes {
		out = append(out, choice{Value: string(sc), Label: tr.T(scopeKeys[sc])})
	}
	return out
}

func providerChoices(tr *i18n.Translator) []choice {
	providers := mirror.Providers()

	out := make([]choice, 0, len(providers))
	for _, p := range providers {
		out = append(out, choice{Value: string(p), Label: tr.T(providerKeys[p])})
	}
	return out
}

func trustChoices(tr *i18n.Translator) []choice {
	trusts := mirror.Trusts()

	out := make([]choice, 0, len(trusts))
	for _, t := range trusts {
		out = append(out, choice{Value: string(t), Label: tr.T(trustKeys[t])})
	}
	return out
}
