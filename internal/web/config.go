package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/DC1024/mirrorpilot/internal/configgen"
	"github.com/DC1024/mirrorpilot/internal/i18n"
	"github.com/DC1024/mirrorpilot/internal/relay"
	"github.com/DC1024/mirrorpilot/internal/store"
)

// configWarningKeys names the translation for each caveat configgen reports.
//
// Written out rather than built from the code, for the same reason the status
// tables are: a constructed key is invisible to the check that proves every
// shipped string is actually shown, so the translations would quietly become
// dead weight.
var configWarningKeys = map[configgen.WarningCode]string{
	configgen.WarningUnmeasured: "config.warn.unmeasured",
	configgen.WarningInsecure:   "config.warn.insecure",
	configgen.WarningDuplicated: "config.warn.duplicated",
}

// containerdPath is where the generated hosts.toml belongs.
//
// Named here rather than only in the file's own header comment, because the
// page has to say it too: a document that is only correct in one directory is
// a document whose filename is not the whole story.
const containerdPath = "/etc/containerd/certs.d/docker.io/hosts.toml"

// configMirrorView is one mirror as the configuration page ranks it.
type configMirrorView struct {
	// Rank is the position in the generated file, counting from one.
	Rank int

	Name     string
	Endpoint string

	// Throughput is the newest measured rate, empty when nothing was measured.
	Throughput string

	Insecure   bool
	Unmeasured bool
}

// configWarning is one caveat about the mirrors going into the file.
type configWarning struct {
	// Message is already translated.
	Message string

	// Names are the mirrors the caveat is about.
	Names []string
}

// relayView is the relay address builder's state.
type relayView struct {
	Endpoint string
	Image    string

	// Address and Command are the built results, empty until asked for.
	Address string
	Command string

	// Error is the translated reason the address could not be built and
	// Detail the validator's own wording, shown as-is for the same reason the
	// mirror form shows it: it names the offending part precisely.
	Error  string
	Detail string

	// Origins are the upstreams offered as a starting point.
	Origins []string
}

// configPage backs the configuration page.
type configPage struct {
	// Mirrors is the ranked list as it will appear in the file.
	Mirrors []configMirrorView

	Warnings []configWarning

	// Candidates is how many mirrors went in, and Excluded how many were left
	// out because registry-mirrors does not cover them.
	Candidates int
	Excluded   int

	// Existing is the daemon.json the visitor pasted, echoed back into the box
	// so a rejection costs them a correction rather than the whole document.
	Existing string

	// ExistingError is the translated reason a pasted document was refused.
	ExistingError string

	// DaemonJSON is the generated file, or DaemonError the reason there is
	// none.
	DaemonJSON  string
	DaemonError string

	// ContainerdTOML is the generated hosts.toml, empty when there is nothing
	// to generate from — the case the mirrors card above already explains.
	ContainerdTOML string

	// ContainerdPath is where the TOML belongs.
	ContainerdPath string

	Relay relayView
}

// handleConfig renders the configuration the measured mirrors imply.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	page, err := s.loadConfigPage(r.Context(), "", r.URL.Query().Get("image"))
	if err != nil {
		s.fail(w, r, "web: load configuration", err)
		return
	}

	s.renderConfig(w, r, page, http.StatusOK)
}

// handleConfigMerge re-renders the page with a pasted daemon.json merged in.
//
// The pasted document is not stored. It is the visitor's own file, it is used
// once to produce an answer they copy back out, and keeping a copy of someone's
// /etc/docker/daemon.json in our database would be hoarding for no reason.
func (s *Server) handleConfigMerge(w http.ResponseWriter, r *http.Request) {
	page, err := s.loadConfigPage(r.Context(), r.PostFormValue("existing"), "")
	if err != nil {
		s.fail(w, r, "web: merge daemon.json", err)
		return
	}

	// A 400 when the pasted document was the problem, so a caching proxy does
	// not serve the refusal back, and so the status says what happened.
	status := http.StatusOK
	if page.ExistingError != "" {
		status = http.StatusBadRequest
	}

	s.renderConfig(w, r, page, status)
}

// renderConfig writes the configuration page.
func (s *Server) renderConfig(w http.ResponseWriter, r *http.Request, page *configPage, status int) {
	data := s.newPageData(r, stateFrom(r.Context()), "config.title", "nav.config")
	data.ConfigPage = page

	s.render(w, r, "config", status, data)
}

// loadConfigPage assembles everything the configuration page shows.
func (s *Server) loadConfigPage(ctx context.Context, existing, image string) (*configPage, error) {
	tr := s.i18n.Translator(stateFrom(ctx).Locale)

	candidates, excluded, err := s.configCandidates(ctx)
	if err != nil {
		return nil, err
	}

	page := &configPage{
		Candidates:     len(candidates),
		Excluded:       excluded,
		Existing:       existing,
		ContainerdPath: containerdPath,
		Relay:          s.relayView(ctx),
	}

	// Ranked and warned here rather than taken from DaemonJSON's return value.
	// Select is pure and deterministic over its input, so these describe
	// exactly the document produced below; re-deriving them keeps
	// DaemonJSON's signature about the file it returns.
	ranked, warnings := configgen.Select(candidates)

	page.Mirrors = newConfigMirrors(ranked)
	page.Warnings = newConfigWarnings(warnings, tr)

	daemon, _, err := configgen.DaemonJSON(candidates, existing)
	switch {
	case err == nil:
		page.DaemonJSON = daemon
	case errors.Is(err, configgen.ErrNoMirrors):
		page.DaemonError = tr.T("config.error.no_mirrors")
	case errors.Is(err, configgen.ErrExisting):
		// The validator's wording names the mistake — a comment, a trailing
		// comma — far better than a translation could.
		page.ExistingError = tr.T("config.error.existing")
	default:
		return nil, err
	}

	// The two generators refuse the same condition — nothing to configure —
	// and the mirrors card has already said so, so an empty result here is not
	// given a second message of its own.
	toml, _, err := configgen.ContainerdHostsTOML(candidates)
	switch {
	case err == nil:
		page.ContainerdTOML = toml
	case errors.Is(err, configgen.ErrNoMirrors):
		// Left empty.
	default:
		return nil, err
	}

	buildRelayAddress(&page.Relay, image, tr)

	return page, nil
}

// configCandidates builds the list the generators rank.
//
// Enabled mirrors that proxy Docker Hub, each carrying its newest measurement.
// A switched-off mirror is not a candidate: the enable flag is how the user
// says which mirrors they are willing to use, and writing a switched-off one
// into their daemon.json would make the flag a decoration.
func (s *Server) configCandidates(ctx context.Context) ([]configgen.Mirror, int, error) {
	records, err := s.store.ListSources(ctx)
	if err != nil {
		return nil, 0, err
	}
	latest, err := s.store.LatestProbes(ctx)
	if err != nil {
		return nil, 0, err
	}

	out := make([]configgen.Mirror, 0, len(records))
	excluded := 0

	for _, rec := range records {
		if !rec.Enabled {
			continue
		}
		// Counted rather than silently dropped, because someone who has
		// enabled a ghcr.io mirror and does not see it in the file would
		// otherwise conclude the tool had lost it.
		if !servesDockerHub(rec) {
			excluded++
			continue
		}

		out = append(out, configgen.Mirror{
			Name:     rec.Name,
			URL:      rec.URL,
			Insecure: rec.Insecure,
			BPS:      latest[rec.ID].ThroughputBPS,
		})
	}

	return out, excluded, nil
}

// relayView assembles the address builder's initial state.
func (s *Server) relayView(ctx context.Context) relayView {
	endpoint, err := s.store.SettingOrDefault(ctx, store.SettingRelayEndpoint, "")
	if err != nil {
		// A failed settings read is a database problem, and the builder
		// degrades to being empty rather than taking the page down: the
		// generated files above it are what most visitors came for.
		s.log.ErrorContext(ctx, "web: read relay endpoint", "err", err)
		endpoint = ""
	}

	return relayView{
		Endpoint: endpoint,
		Origins:  relay.Origins(),
	}
}

// buildRelayAddress fills in the built address when an image was given.
//
// Only built when asked. Deriving a pull address for every visitor who never
// asked for one would put a second generated document on a page that already
// has two, and the failure modes here — no endpoint yet, an unparseable
// reference — are all things the visitor has to fix anyway.
func buildRelayAddress(view *relayView, image string, tr *i18n.Translator) {
	image = strings.TrimSpace(image)
	if image == "" {
		return
	}

	view.Image = image

	switch {
	case view.Endpoint == "":
		view.Error = tr.T("config.relay.error.no_endpoint")
		return
	}

	ref, err := relay.Parse(image)
	if err != nil {
		view.Error = tr.T("config.relay.error.image")
		view.Detail = err.Error()
		return
	}

	address, err := relay.Relayed(view.Endpoint, ref)
	if err != nil {
		// Reachable when the stored endpoint was hand-edited into something
		// unusable, so it is worth saying rather than swallowing.
		view.Error = tr.T("config.relay.error.endpoint")
		view.Detail = err.Error()
		return
	}

	view.Address = address
	view.Command = "docker pull " + address
}

// handleRelaySave stores the relay endpoint.
//
// The endpoint is the one durable fact on the builder, so it is saved on its
// own rather than as a side effect of building an address: someone who has
// registered a relay host and not yet decided what to pull has still told us
// something worth keeping.
func (s *Server) handleRelaySave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tr := s.i18n.Translator(stateFrom(ctx).Locale)

	endpoint := strings.TrimSpace(r.PostFormValue("relay_endpoint"))

	normalised, err := relay.Endpoint(endpoint)
	if err != nil && endpoint != "" {
		page, loadErr := s.loadConfigPage(ctx, "", "")
		if loadErr != nil {
			s.fail(w, r, "web: load configuration", loadErr)
			return
		}

		page.Relay.Endpoint = endpoint
		page.Relay.Error = tr.T("config.relay.error.endpoint")
		page.Relay.Detail = err.Error()

		s.renderConfig(w, r, page, http.StatusBadRequest)
		return
	}

	if endpoint == "" {
		if err := s.store.DeleteSetting(ctx, store.SettingRelayEndpoint); err != nil {
			s.fail(w, r, "web: clear relay endpoint", err)
			return
		}
		redirect(w, r, "/config?flash=config.relay.cleared")
		return
	}

	if err := s.store.SetSetting(ctx, store.SettingRelayEndpoint, normalised); err != nil {
		s.fail(w, r, "web: save relay endpoint", err)
		return
	}

	redirect(w, r, "/config?flash=config.relay.saved")
}

// newConfigMirrors renders the ranked list for the page.
func newConfigMirrors(ranked []configgen.Mirror) []configMirrorView {
	out := make([]configMirrorView, 0, len(ranked))

	for i, m := range ranked {
		out = append(out, configMirrorView{
			Rank:       i + 1,
			Name:       m.Name,
			Endpoint:   m.Endpoint(),
			Throughput: humanRate(m.BPS),
			Insecure:   m.Insecure,
			Unmeasured: m.BPS <= 0,
		})
	}
	return out
}

// newConfigWarnings translates the caveats configgen reported.
func newConfigWarnings(warnings []configgen.Warning, tr *i18n.Translator) []configWarning {
	out := make([]configWarning, 0, len(warnings))

	for _, w := range warnings {
		key, ok := configWarningKeys[w.Code]
		if !ok {
			// A code from a newer configgen than this page knows about.
			// Showing the raw code beats showing nothing, and beats a sentence
			// the panel would have had to invent.
			out = append(out, configWarning{Message: string(w.Code), Names: w.Names})
			continue
		}
		out = append(out, configWarning{Message: tr.T(key), Names: w.Names})
	}
	return out
}
