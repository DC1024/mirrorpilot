package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DC1024/mirrorpilot/internal/mirror"
)

// The platform the tests run on, phrased as constants so a fixture can claim
// to be for "this machine" without the test guessing wrong.
const (
	hostOS   = runtime.GOOS
	hostArch = runtime.GOARCH
)

// digestOf computes a digest the way the probe does: from the bytes, never
// from a header. Fixtures build their digests the same way, so a test can only
// pass by actually transporting the right content.
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// stub is a minimal registry that answers just enough of the distribution
// protocol to drive the four layers. Every knob defaults to the boring case,
// so a test only states the one thing it is about.
type stub struct {
	// connect is the status for GET /v2/.
	connect      int
	connectRetry string

	// challengePath, when set, makes /v2/ answer with a WWW-Authenticate
	// Bearer challenge whose realm is this path on the stub's own host — the
	// same shape as a real registry, whose token endpoint is usually elsewhere
	// than the mirror path itself. challengeHost overrides the host in that
	// realm, which is how a test points it at a server that is not listening.
	challengePath string
	challengeHost string

	// token is the status for GET <tokenPath>. Zero means 404 — a mirror with
	// no token endpoint, which is a legitimate, working configuration.
	token     int
	tokenBody string
	// tokenPath is where the token endpoint lives. Defaults to /token.
	tokenPath string

	// manifests is keyed by reference, which for a digest-pinned fetch is the
	// digest itself.
	manifests map[string]stubManifest

	// requireBearer, when set, makes every manifest answer 401 unless the
	// request carries exactly this bearer token.
	requireBearer string

	blobStatus int
	blobRetry  string
	blobs      map[string][]byte

	// delay is applied to every request, for timeout and concurrency tests.
	delay time.Duration
}

// stubManifest is one manifest the fake registry serves.
type stubManifest struct {
	body      []byte
	status    int
	mediaType string

	// declared is emitted as Docker-Content-Digest. Setting it to something
	// other than the body's real digest reproduces a mirror that lies.
	declared string
}

// requestLog records what a probe actually asked for. Without it, a test can
// only see the verdict and not the behaviour that produced it — and "did not
// press on after being told to back off" is a behaviour worth asserting.
type requestLog struct {
	mu    sync.Mutex
	paths []string
	cur   int
	peak  int
}

func (l *requestLog) enter() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur++
	if l.cur > l.peak {
		l.peak = l.cur
	}
}

func (l *requestLog) leave() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur--
}

func (l *requestLog) add(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, path)
}

func (l *requestLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.paths)
}

func (l *requestLog) peakConcurrent() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peak
}

// newStub starts the fake registry and returns a source pointing at it.
func newStub(t *testing.T, s stub) (*httptest.Server, mirror.Source, *requestLog) {
	t.Helper()

	if s.tokenPath == "" {
		s.tokenPath = "/token"
	}

	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.enter()
		defer log.leave()
		log.add(r.URL.Path)

		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		serveStub(w, r, s)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv, mirror.Source{ID: "stub", Name: "Stub", URL: srv.URL}, log
}

func serveStub(w http.ResponseWriter, r *http.Request, s stub) {
	switch r.URL.Path {
	case "/v2/":
		if s.connectRetry != "" {
			w.Header().Set("Retry-After", s.connectRetry)
		}
		if s.challengePath != "" {
			s.setChallengeHeader(w, r)
		}
		w.WriteHeader(orStatus(s.connect, http.StatusOK))
		return
	case s.tokenPath:
		if status := orStatus(s.token, http.StatusNotFound); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		body := s.tokenBody
		if body == "" {
			body = `{"token":"stub-token"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
		return
	}

	if ref, ok := afterMarker(r.URL.Path, "/manifests/"); ok {
		serveManifest(w, r, ref, s)
		return
	}
	if dig, ok := afterMarker(r.URL.Path, "/blobs/"); ok {
		serveBlob(w, r, dig, s)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// setChallengeHeader emits the WWW-Authenticate a real registry attaches to
// every 401, not just the one on /v2/ — a challenge can arrive mid-probe, and
// that is exactly the case the re-authorisation logic exists for.
func (s stub) setChallengeHeader(w http.ResponseWriter, r *http.Request) {
	if s.challengePath == "" {
		return
	}
	realmHost := orString(s.challengeHost, r.Host)
	w.Header().Set("WWW-Authenticate",
		fmt.Sprintf(`Bearer realm="http://%s%s", service="stub"`, realmHost, s.challengePath))
}

func serveManifest(w http.ResponseWriter, r *http.Request, ref string, s stub) {
	if s.requireBearer != "" && r.Header.Get("Authorization") != "Bearer "+s.requireBearer {
		s.setChallengeHeader(w, r)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	m, ok := s.manifests[ref]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if status := orStatus(m.status, http.StatusOK); status != http.StatusOK {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			s.setChallengeHeader(w, r)
		}
		w.WriteHeader(status)
		return
	}
	if m.declared != "" {
		w.Header().Set("Docker-Content-Digest", m.declared)
	}
	w.Header().Set("Content-Type", orString(m.mediaType, "application/vnd.oci.image.manifest.v1+json"))
	_, _ = w.Write(m.body)
}

func serveBlob(w http.ResponseWriter, r *http.Request, dig string, s stub) {
	if s.requireBearer != "" && r.Header.Get("Authorization") != "Bearer "+s.requireBearer {
		s.setChallengeHeader(w, r)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if status := orStatus(s.blobStatus, http.StatusOK); status != http.StatusOK {
		if s.blobRetry != "" {
			w.Header().Set("Retry-After", s.blobRetry)
		}
		w.WriteHeader(status)
		return
	}
	body, ok := s.blobs[dig]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = w.Write(body)
}

// afterMarker returns whatever follows marker in path: the reference of a
// manifest, or the digest of a blob.
func afterMarker(path, marker string) (string, bool) {
	_, rest, ok := strings.Cut(path, marker)
	return rest, ok
}

func orStatus(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func orString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// --- fixture builders ---------------------------------------------------

type testPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

type testDescriptor struct {
	MediaType string        `json:"mediaType,omitempty"`
	Digest    string        `json:"digest"`
	Size      int64         `json:"size"`
	Platform  *testPlatform `json:"platform,omitempty"`
}

type testIndex struct {
	SchemaVersion int              `json:"schemaVersion"`
	MediaType     string           `json:"mediaType"`
	Manifests     []testDescriptor `json:"manifests"`
}

type testImage struct {
	SchemaVersion int              `json:"schemaVersion"`
	MediaType     string           `json:"mediaType"`
	Config        testDescriptor   `json:"config"`
	Layers        []testDescriptor `json:"layers"`
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// plainSource is a mirror with nothing but the fields the probe reads.
func plainSource(url string) mirror.Source {
	return mirror.Source{ID: "stub", Name: "Stub", URL: url}
}

func defaultTarget() Target {
	return Target{Repository: "library/alpine", Reference: "latest"}
}

// newProber builds a Prober that talks to an httptest server, with tunables
// applied on top of the defaults.
//
// A nil server means the caller wants the same bare transport anyway. Either
// way the transport is a fresh, empty one, so neither a proxy from the
// environment nor a client-level timeout can quietly change what a test
// measures.
func newProber(t *testing.T, srv *httptest.Server, tweak func(*Options)) *Prober {
	t.Helper()

	client := &http.Client{Transport: &http.Transport{}}
	if srv != nil {
		client = srv.Client()
	}

	opts := Options{Target: defaultTarget(), Client: client}
	if tweak != nil {
		tweak(&opts)
	}

	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// --- layer 1: connectivity ---------------------------------------------

func TestProbeRejectsMirrorWithoutURL(t *testing.T) {
	p, err := New(Options{Target: defaultTarget()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res := p.Probe(t.Context(), mirror.Source{ID: "broken"})

	if res.Status != StatusFailed {
		t.Errorf("status = %q, want %q", res.Status, StatusFailed)
	}
	if res.Detail != "mirror has no URL" {
		t.Errorf("detail = %q", res.Detail)
	}
}

func TestProbeUnreachableMirror(t *testing.T) {
	// Start a server and stop it, so the port is guaranteed to refuse rather
	// than merely being unusual.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	p, err := New(Options{Target: defaultTarget(), ConnectTimeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res := p.Probe(t.Context(), plainSource(url))

	if res.Status != StatusUnreachable {
		t.Fatalf("status = %q, want %q (detail %q)", res.Status, StatusUnreachable, res.Detail)
	}
	if res.Detail == "" {
		t.Error("an unreachable mirror should explain why")
	}
	if res.Connect.Millis() < 0 {
		t.Errorf("connect millis = %d", res.Connect.Millis())
	}
}

func TestProbeTimesOutRatherThanHanging(t *testing.T) {
	srv, src, _ := newStub(t, stub{delay: 2 * time.Second})

	p := newProber(t, srv, func(o *Options) {
		o.Timeout = 80 * time.Millisecond
		o.ConnectTimeout = time.Second
	})

	res := p.Probe(t.Context(), src)

	if res.Status != StatusUnreachable {
		t.Fatalf("status = %q, want %q", res.Status, StatusUnreachable)
	}
	if res.Detail != "timed out" {
		t.Errorf("detail = %q, want %q", res.Detail, "timed out")
	}
}

func TestProbeRateLimitedAtConnectStopsThere(t *testing.T) {
	_, src, log := newStub(t, stub{connect: http.StatusTooManyRequests, connectRetry: "30"})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Status != StatusRateLimited {
		t.Fatalf("status = %q, want %q", res.Status, StatusRateLimited)
	}
	if !contains(res.Detail, "30") {
		t.Errorf("detail = %q, want it to mention the retry delay", res.Detail)
	}
	// The point of the test: a mirror that said "back off" was asked once.
	if got := log.count(); got != 1 {
		t.Errorf("made %d requests, want 1: %v", got, log.paths)
	}
}

// A 401 without a realm is a wall: nothing was advertised for an anonymous
// client to follow, so there is no step that would let us in.
func TestProbeConnectUnauthorizedWithoutAChallengeIsACredentialWall(t *testing.T) {
	blob := bytes.Repeat([]byte("x"), 1024)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	_, src, _ := newStub(t, stub{
		connect:   http.StatusUnauthorized,
		manifests: map[string]stubManifest{"latest": {body: child}},
		blobs:     map[string][]byte{digestOf(blob): blob},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Connect.Status != StatusUnauthorized {
		t.Errorf("connect = %q, want %q", res.Connect.Status, StatusUnauthorized)
	}
	if !contains(res.Connect.Detail, "token realm") {
		t.Errorf("connect detail = %q, want it to say no realm was advertised", res.Connect.Detail)
	}
	// Alive still holds: the host answered and spoke the protocol, and here
	// the layers below succeed without ever asking for a token.
	if res.Status != StatusOK {
		t.Errorf("status = %q, want %q (detail %q)", res.Status, StatusOK, res.Detail)
	}
}

// The ordinary handshake: /v2/ answers 401 with a Bearer realm, which is what
// every anonymous pull starts with. Reporting that as a credential wall is
// what made the page describe working mirrors as locked.
func TestProbeConnectAnsweringAChallengeIsNotACredentialWall(t *testing.T) {
	blob := bytes.Repeat([]byte("x"), 1024)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	srv, src, _ := newStub(t, stub{
		connect:       http.StatusUnauthorized,
		challengePath: "/auth/token",
		tokenPath:     "/auth/token",
		token:         http.StatusOK,
		manifests:     map[string]stubManifest{"latest": {body: child}},
		blobs:         map[string][]byte{digestOf(blob): blob},
		requireBearer: "stub-token",
	})
	defer srv.Close()

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Connect.Status != StatusOK {
		t.Fatalf("connect = %q (%q), want %q", res.Connect.Status, res.Connect.Detail, StatusOK)
	}
	// The realm is not the mirror itself in real life, so the note has to
	// name where the token will come from.
	if !contains(res.Connect.Detail, strings.TrimPrefix(srv.URL, "http://")) {
		t.Errorf("connect detail = %q, want it to name the realm host", res.Connect.Detail)
	}
	if !contains(res.Connect.Detail, "anonymous") {
		t.Errorf("connect detail = %q, want it to say the pull stays anonymous", res.Connect.Detail)
	}
	if res.Status != StatusOK {
		t.Errorf("status = %q, want %q (detail %q)", res.Status, StatusOK, res.Detail)
	}
}

// --- layer 2: token ----------------------------------------------------

// The challenge-following tests all use the same shape: a mirror that answers
// 401 on /v2/ and advertises its token endpoint somewhere other than /token —
// which is what real mirrors do, and what the engine used to get wrong.

func TestProbeFetchesTheTokenFromTheAdvertisedRealm(t *testing.T) {
	blob := bytes.Repeat([]byte("x"), 1024)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	_, src, log := newStub(t, stub{
		connect:       http.StatusUnauthorized,
		challengePath: "/auth/token",
		tokenPath:     "/auth/token",
		token:         http.StatusOK,
		manifests:     map[string]stubManifest{"latest": {body: child}},
		blobs:         map[string][]byte{digestOf(blob): blob},
		// The manifest is only served to the token the realm issued, so this
		// test passes only if the token really made it onto the request.
		requireBearer: "stub-token",
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Token.Status != StatusOK {
		t.Errorf("token = %q (%q), want %q", res.Token.Status, res.Token.Detail, StatusOK)
	}
	if res.Status != StatusOK {
		t.Fatalf("status = %q, want %q (detail %q)", res.Status, StatusOK, res.Detail)
	}
	if !log.sawPath("/auth/token") {
		t.Errorf("the advertised realm was never asked for a token: %v", log.paths)
	}
	if log.sawPath("/token") {
		t.Errorf("the invented /token endpoint was still asked: %v", log.paths)
	}
}

func TestProbeWithoutAChallengeDoesNotAskForAToken(t *testing.T) {
	blob := bytes.Repeat([]byte("y"), 2048)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	// /v2/ answers 200 — no challenge — yet a /token endpoint exists. Asking
	// it anyway is a guess, and a 401 from a guess used to read as the mirror
	// demanding credentials.
	_, src, log := newStub(t, stub{
		manifests: map[string]stubManifest{"latest": {body: child}},
		blobs:     map[string][]byte{digestOf(blob): blob},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Token.Status != StatusSkipped {
		t.Errorf("token = %q, want %q — nobody asked for a token", res.Token.Status, StatusSkipped)
	}
	if log.sawPath("/token") {
		t.Errorf("a token was requested although no challenge was advertised: %v", log.paths)
	}
	if res.Status != StatusOK {
		t.Fatalf("status = %q, want %q (detail %q)", res.Status, StatusOK, res.Detail)
	}
}

func TestProbeUnreachableRealmIsReportedAtTheTokenLayer(t *testing.T) {
	// A realm that points at a server which has stopped: the challenge was
	// advertised honestly, but nothing answers there.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadHost := dead.URL
	dead.Close()

	_, src, _ := newStub(t, stub{
		connect:       http.StatusUnauthorized,
		challengePath: "/auth/token",
		challengeHost: strings.TrimPrefix(deadHost, "http://"),
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Token.Status != StatusUnreachable {
		t.Errorf("token = %q, want %q", res.Token.Status, StatusUnreachable)
	}
}

func TestProbeTokenEndpointMissingIsSkippedNotFailed(t *testing.T) {
	blob := bytes.Repeat([]byte("y"), 2048)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	_, src, _ := newStub(t, stub{
		connect:       http.StatusUnauthorized,
		challengePath: "/auth/token",
		tokenPath:     "/auth/token",
		token:         http.StatusNotFound,
		manifests:     map[string]stubManifest{"latest": {body: child}},
		blobs:         map[string][]byte{digestOf(blob): blob},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Token.Status != StatusSkipped {
		t.Errorf("token = %q, want %q", res.Token.Status, StatusSkipped)
	}
	if res.Status != StatusOK {
		t.Fatalf("status = %q, want %q (detail %q)", res.Status, StatusOK, res.Detail)
	}
	if !contains(res.Detail, "without requesting a token") {
		t.Errorf("detail = %q, want the measurement caveat", res.Detail)
	}
}

func TestProbeRateLimitedAtToken(t *testing.T) {
	_, src, log := newStub(t, stub{
		connect:       http.StatusUnauthorized,
		challengePath: "/auth/token",
		tokenPath:     "/auth/token",
		token:         http.StatusTooManyRequests,
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Token.Status != StatusRateLimited {
		t.Errorf("token = %q, want %q", res.Token.Status, StatusRateLimited)
	}
	if res.Status != StatusRateLimited {
		t.Errorf("status = %q, want %q", res.Status, StatusRateLimited)
	}
	// The challenge and the token, and nothing else.
	if got := log.count(); got != 2 {
		t.Errorf("made %d requests, want 2: %v", got, log.paths)
	}
}

func TestProbeTokenResponseWithoutTokenFails(t *testing.T) {
	_, src, _ := newStub(t, stub{
		connect:       http.StatusUnauthorized,
		challengePath: "/auth/token",
		tokenPath:     "/auth/token",
		token:         http.StatusOK,
		tokenBody:     `{"expires_in":300}`,
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Token.Status != StatusFailed {
		t.Errorf("token = %q, want %q", res.Token.Status, StatusFailed)
	}
	if !contains(res.Token.Detail, "no token") {
		t.Errorf("token detail = %q", res.Token.Detail)
	}
}

func TestProbeAcceptsAccessTokenSpelling(t *testing.T) {
	blob := []byte("z")
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	_, src, _ := newStub(t, stub{
		connect:       http.StatusUnauthorized,
		challengePath: "/auth/token",
		tokenPath:     "/auth/token",
		token:         http.StatusOK,
		tokenBody:     `{"access_token":"other-spelling"}`, //nolint:gosec // test fixture
		manifests:     map[string]stubManifest{"latest": {body: child}},
		blobs:         map[string][]byte{digestOf(blob): blob},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Token.Status != StatusOK {
		t.Errorf("token = %q, want %q (detail %q)", res.Token.Status, StatusOK, res.Token.Detail)
	}
}

func TestParseChallenge(t *testing.T) {
	cases := map[string]struct {
		header  string
		realm   string
		service string
	}{
		"quoted values": {
			`Bearer realm="https://m.daocloud.io/auth/token",service="docker.m.daocloud.io"`,
			"https://m.daocloud.io/auth/token", "docker.m.daocloud.io",
		},
		"spaces after commas": {
			`Bearer realm="https://r/token", service="svc", scope="repository:x:pull"`,
			"https://r/token", "svc",
		},
		"unquoted values": {
			`Bearer realm=https://r/token,service=svc`,
			"https://r/token", "svc",
		},
		"no service": {
			`Bearer realm="https://r/token"`,
			"https://r/token", "",
		},
		"lowercase scheme": {
			`bearer realm="https://r/token"`,
			"https://r/token", "",
		},
		"not a bearer challenge": {
			`Basic realm="https://r/"`,
			"", "",
		},
		"garbage": {
			"anything at all",
			"", "",
		},
		"empty": {
			"",
			"", "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := parseChallenge(tc.header)
			if got.realm != tc.realm || got.service != tc.service {
				t.Errorf("parseChallenge = %+v, want realm %q service %q", got, tc.realm, tc.service)
			}
		})
	}
}

func TestProbeReauthorizesAfterARedirectStrippedTheToken(t *testing.T) {
	// hub.rat.dev is a real example: an alias that 302s everything to another
	// registry. A redirect across hosts strips the Authorization header, so
	// every authenticated request lands as anonymous and is challenged again.
	// A real client re-fetches a token for the new challenge and retries
	// against where it actually landed; the probe now does the same, and this
	// test fails unless it does.
	blob := bytes.Repeat([]byte("r"), 8192)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	srvB, _, logB := newStub(t, stub{
		connect:       http.StatusUnauthorized,
		challengePath: "/auth/token",
		tokenPath:     "/auth/token",
		token:         http.StatusOK,
		manifests:     map[string]stubManifest{"latest": {body: child}},
		blobs:         map[string][]byte{digestOf(blob): blob},
		requireBearer: "stub-token",
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Redirect by a different host name — 127.0.0.1 to localhost — because
		// that is what makes Go's client strip the Authorization header, and
		// that stripping is the behaviour under test.
		target := strings.Replace(srvB.URL, "127.0.0.1", "localhost", 1)
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusFound)
	})
	srvA := httptest.NewServer(mux)
	t.Cleanup(srvA.Close)

	res := newProber(t, nil, nil).Probe(t.Context(), mirror.Source{ID: "alias", Name: "Alias", URL: srvA.URL})

	if res.Manifest.Status != StatusOK {
		t.Errorf("manifest = %q (%q), want a retry to succeed", res.Manifest.Status, res.Manifest.Detail)
	}
	if res.Status != StatusOK {
		t.Fatalf("status = %q, want %q (detail %q)", res.Status, StatusOK, res.Detail)
	}
	if !logB.sawPath("/auth/token") {
		t.Errorf("the redirected registry's realm was never asked for a token")
	}
}

// --- layer 3: manifest -------------------------------------------------

func TestProbeManifestDigestLieIsCaught(t *testing.T) {
	blob := []byte("payload")
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	_, src, _ := newStub(t, stub{
		manifests: map[string]stubManifest{"latest": {
			body: child,
			// The mirror claims a digest the bytes do not hash to.
			declared: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		}},
		blobs: map[string][]byte{digestOf(blob): blob},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Manifest.Status != StatusFailed {
		t.Errorf("manifest = %q, want %q", res.Manifest.Status, StatusFailed)
	}
	if !contains(res.Manifest.Detail, "declared one manifest digest") {
		t.Errorf("manifest detail = %q", res.Manifest.Detail)
	}
	if res.Status != StatusFailed {
		t.Errorf("status = %q, want %q", res.Status, StatusFailed)
	}
}

func TestProbeManifestUnauthorized(t *testing.T) {
	_, src, _ := newStub(t, stub{
		manifests: map[string]stubManifest{"latest": {status: http.StatusUnauthorized}},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Manifest.Status != StatusUnauthorized {
		t.Errorf("manifest = %q, want %q", res.Manifest.Status, StatusUnauthorized)
	}
	if res.Status != StatusUnauthorized {
		t.Errorf("status = %q, want %q", res.Status, StatusUnauthorized)
	}
}

func TestProbeManifestMissing(t *testing.T) {
	_, src, _ := newStub(t, stub{})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Manifest.Status != StatusFailed {
		t.Errorf("manifest = %q, want %q", res.Manifest.Status, StatusFailed)
	}
	if !contains(res.Manifest.Detail, "404") {
		t.Errorf("manifest detail = %q, want the status code", res.Manifest.Detail)
	}
}

func TestProbeFollowsMultiPlatformIndex(t *testing.T) {
	blob := bytes.Repeat([]byte("m"), 4096)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers: []testDescriptor{
			{Digest: digestOf([]byte("small")), Size: 5},
			{Digest: digestOf(blob), Size: int64(len(blob))},
		},
	})

	index := mustJSON(t, testIndex{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []testDescriptor{
			{
				Digest: digestOf(child), Size: int64(len(child)),
				Platform: &testPlatform{OS: hostOS, Architecture: hostArch},
			},
			{
				Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Size:   1,
				Platform: &testPlatform{
					OS: "plan9", Architecture: "mips",
				},
			},
		},
	})

	_, src, log := newStub(t, stub{
		manifests: map[string]stubManifest{
			"latest":        {body: index},
			digestOf(child): {body: child},
		},
		blobs: map[string][]byte{
			digestOf(blob):            blob,
			digestOf([]byte("small")): []byte("small"),
		},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Status != StatusOK {
		t.Fatalf("status = %q, want %q (detail %q)", res.Status, StatusOK, res.Detail)
	}
	// The largest layer was measured, not the first one listed.
	if res.Bytes != int64(len(blob)) {
		t.Errorf("bytes = %d, want %d (the largest layer)", res.Bytes, len(blob))
	}
	// The index was followed by digest, which is what keeps a probe
	// reproducible when a tag moves.
	if !log.sawPath("/v2/library/alpine/manifests/sha256:") {
		t.Errorf("did not re-fetch the child manifest by digest: %v", log.paths)
	}
}

func TestProbeIndexWithoutOurPlatformIsSkipped(t *testing.T) {
	index := mustJSON(t, testIndex{
		MediaType: "application/vnd.oci.image.index.v1+json",
		Manifests: []testDescriptor{{
			Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			Size:   1,
			Platform: &testPlatform{
				OS: "plan9", Architecture: "mips",
			},
		}},
	})

	_, src, _ := newStub(t, stub{
		manifests: map[string]stubManifest{"latest": {body: index}},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Throughput.Status != StatusSkipped {
		t.Errorf("throughput = %q, want %q", res.Throughput.Status, StatusSkipped)
	}
	if res.Status != StatusSkipped {
		t.Errorf("status = %q, want %q", res.Status, StatusSkipped)
	}
	if res.BPS() != 0 {
		t.Errorf("bps = %d, want 0 — nothing was measured", res.BPS())
	}
}

// --- layer 4: throughput -----------------------------------------------

func TestProbeMeasuresThroughputAndVerifiesContent(t *testing.T) {
	blob := bytes.Repeat([]byte("throughput"), 64*1024)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	_, src, _ := newStub(t, stub{
		manifests: map[string]stubManifest{"latest": {body: child}},
		blobs:     map[string][]byte{digestOf(blob): blob},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Status != StatusOK {
		t.Fatalf("status = %q, want %q (detail %q)", res.Status, StatusOK, res.Detail)
	}
	if res.Bytes != int64(len(blob)) {
		t.Errorf("bytes = %d, want %d", res.Bytes, len(blob))
	}
	if !res.BlobDigestChecked {
		t.Error("a complete read should have been checkable")
	}
	if !res.BlobDigestOK {
		t.Error("content matching its digest should verify")
	}
	if res.Throughput.Status != StatusOK {
		t.Errorf("throughput = %q, want %q", res.Throughput.Status, StatusOK)
	}
	if res.Throughput.Duration < 0 {
		t.Errorf("duration = %v, want a non-negative measurement", res.Throughput.Duration)
	}
	// The rate itself is asserted in TestBPSDoesNotInventAMeasuredZero rather
	// than here, because this machine's monotonic clock reports an entire
	// localhost HTTP round trip as zero. BPS is a pure function of bytes and
	// duration, so it is tested with a duration we control instead of one the
	// host decides.
	if res.ResolvedDigest != digestOf(child) {
		t.Errorf("resolved digest = %q, want the digest of the manifest served", res.ResolvedDigest)
	}
}

func TestProbeCappedReadIsNotAFailure(t *testing.T) {
	blob := bytes.Repeat([]byte("c"), 256*1024)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	_, src, _ := newStub(t, stub{
		manifests: map[string]stubManifest{"latest": {body: child}},
		blobs:     map[string][]byte{digestOf(blob): blob},
	})

	const cap = 16 * 1024
	res := newProber(t, nil, func(o *Options) { o.MaxBlobBytes = cap }).Probe(t.Context(), src)

	if res.Status != StatusOK {
		t.Fatalf("status = %q, want %q — a cap of ours is not the mirror's fault (detail %q)",
			res.Status, StatusOK, res.Detail)
	}
	if res.Bytes != cap {
		t.Errorf("bytes = %d, want the cap %d", res.Bytes, cap)
	}
	// The important distinction: unverifiable is not the same as wrong.
	if res.BlobDigestChecked {
		t.Error("a truncated read must not claim to have been checked")
	}
	if res.BlobDigestOK {
		t.Error("a truncated read must not claim to have verified")
	}
	if !contains(res.Throughput.Detail, "capped") {
		t.Errorf("throughput detail = %q, want it to say the read was capped", res.Throughput.Detail)
	}
	if res.Throughput.Status != StatusOK {
		t.Errorf("throughput = %q, want %q — a capped read is still a measurement",
			res.Throughput.Status, StatusOK)
	}
	if res.Throughput.Duration < 0 {
		t.Errorf("duration = %v, want a non-negative measurement", res.Throughput.Duration)
	}
	// No BPS assertion here for the same reason as above: this host's clock
	// cannot resolve the transfer, and BPS correctly reports "not measured"
	// rather than inventing a number.
}

func TestProbeBlobContentMismatchFails(t *testing.T) {
	honest := bytes.Repeat([]byte("h"), 8192)
	lie := bytes.Repeat([]byte("L"), 8192)

	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(honest), Size: int64(len(honest))}},
	})

	_, src, _ := newStub(t, stub{
		manifests: map[string]stubManifest{"latest": {body: child}},
		// Same length, different bytes: the read completes, and only the hash
		// can tell the difference.
		blobs: map[string][]byte{digestOf(honest): lie},
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", res.Status, StatusFailed)
	}
	if !contains(res.Detail, "did not match the digest") {
		t.Errorf("detail = %q, want the digest complaint", res.Detail)
	}
	if !res.BlobDigestChecked || res.BlobDigestOK {
		t.Errorf("checked = %v, ok = %v; want checked and not ok", res.BlobDigestChecked, res.BlobDigestOK)
	}
}

func TestProbeRateLimitedAtBlob(t *testing.T) {
	blob := bytes.Repeat([]byte("b"), 4096)
	child := mustJSON(t, testImage{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Layers:    []testDescriptor{{Digest: digestOf(blob), Size: int64(len(blob))}},
	})

	_, src, _ := newStub(t, stub{
		manifests:  map[string]stubManifest{"latest": {body: child}},
		blobStatus: http.StatusTooManyRequests,
		blobRetry:  "7",
	})

	res := newProber(t, nil, nil).Probe(t.Context(), src)

	if res.Status != StatusRateLimited {
		t.Errorf("status = %q, want %q", res.Status, StatusRateLimited)
	}
	if !contains(res.Detail, "7") {
		t.Errorf("detail = %q, want the retry delay", res.Detail)
	}
}

// --- option plumbing ---------------------------------------------------

func TestNewRejectsIncompleteTarget(t *testing.T) {
	cases := map[string]Options{
		"no repository": {Target: Target{Reference: "latest"}},
		"no reference":  {Target: Target{Repository: "library/alpine"}},
		"empty":         {},
		// A repository that is only separators names nothing, and probing it
		// would request "/v2//manifests/latest" — a 404 that gets recorded as
		// the mirror failing rather than as our mistake.
		"slashes only": {Target: Target{Repository: "/", Reference: "latest"}},
		"blank":        {Target: Target{Repository: "   ", Reference: "latest"}},
		"trailing":     {Target: Target{Repository: "library/alpine/", Reference: ""}},
	}

	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Fatal("want an error for an incomplete target")
			}
		})
	}
}

// The rules are shallow on purpose: anything that could plausibly name an image
// is passed through to the mirror, which is the authority on what exists.
func TestNewAcceptsAnyPlausibleTarget(t *testing.T) {
	for _, target := range []Target{
		{Repository: "library/alpine", Reference: "latest"},
		{Repository: "library/alpine", Reference: "sha256:" + strings.Repeat("a", 64)},
		{Repository: "some-user/some-image", Reference: "v1.2.3"},
		{Repository: "  library/alpine  ", Reference: " latest "},
		{Repository: "library/alpine", Reference: "3.20"},
	} {
		if _, err := New(Options{Target: target}); err != nil {
			t.Errorf("New(%+v) = %v, want it accepted", target, err)
		}
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	p, err := New(Options{Target: defaultTarget()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if p.opts.ConnectTimeout != DefaultConnectTimeout {
		t.Errorf("connect timeout = %v, want %v", p.opts.ConnectTimeout, DefaultConnectTimeout)
	}
	if p.opts.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", p.opts.Timeout, DefaultTimeout)
	}
	if p.opts.MaxBlobBytes != DefaultMaxBlobBytes {
		t.Errorf("max blob = %d, want %d", p.opts.MaxBlobBytes, DefaultMaxBlobBytes)
	}
	if p.opts.UserAgent != DefaultUserAgent {
		t.Errorf("user agent = %q, want the default", p.opts.UserAgent)
	}
	if p.Target() != defaultTarget() {
		t.Errorf("target = %+v", p.Target())
	}
}

// --- units -------------------------------------------------------------

func TestStatusAlive(t *testing.T) {
	alive := []Status{StatusOK, StatusUnauthorized, StatusRateLimited}
	dead := []Status{StatusUnreachable, StatusFailed, StatusSkipped}

	for _, s := range alive {
		if !s.Alive() {
			t.Errorf("%q should count as alive", s)
		}
	}
	for _, s := range dead {
		if s.Alive() {
			t.Errorf("%q should not count as alive", s)
		}
	}
}

func TestBPSDoesNotInventAMeasuredZero(t *testing.T) {
	cases := map[string]struct {
		res  Result
		want int64
	}{
		"nothing measured":  {Result{}, 0},
		"bytes but no time": {Result{Bytes: 1024}, 0},
		"time but no bytes": {Result{Throughput: Layer{Duration: 10 * time.Millisecond}}, 0},
		"a real transfer": {
			Result{Bytes: 1000, Throughput: Layer{Duration: 500 * time.Millisecond}},
			2000,
		},
		// The case that made this method stop using integer milliseconds: a
		// transfer too fast to round up to 1ms is not a transfer that did not
		// happen. Reporting zero here would call the fastest mirror broken.
		"a sub-millisecond transfer": {
			Result{Bytes: 64 * 1024, Throughput: Layer{Duration: 200 * time.Microsecond}},
			64 * 1024 * 5000,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.res.BPS(); got != tc.want {
				t.Errorf("bps = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSelectPlatform(t *testing.T) {
	plat := func(os, arch, variant string) *testPlatform {
		return &testPlatform{OS: os, Architecture: arch, Variant: variant}
	}
	desc := func(digest string, p *testPlatform) descriptor {
		d := descriptor{Digest: digest}
		if p != nil {
			d.Platform = &struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
				Variant      string `json:"variant"`
			}{Architecture: p.Architecture, OS: p.OS, Variant: p.Variant}
		}
		return d
	}

	cases := map[string]struct {
		manifests []descriptor
		goos      string
		goarch    string
		want      string
		ok        bool
	}{
		"exact match": {
			[]descriptor{desc("a", plat("linux", "amd64", ""))},
			"linux", "amd64", "a", true,
		},
		"variant is a fallback, plain wins": {
			[]descriptor{desc("v7", plat("linux", "arm", "v7")), desc("plain", plat("linux", "arm", ""))},
			"linux", "arm", "plain", true,
		},
		"variant alone still works": {
			[]descriptor{desc("only", plat("linux", "arm", "v7"))},
			"linux", "arm", "only", true,
		},
		"other platform only": {
			[]descriptor{desc("nope", plat("plan9", "mips", ""))},
			"linux", "arm", "", false,
		},
		"entry with no platform is ignored": {
			[]descriptor{desc("attestation", nil)},
			"linux", "arm", "", false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := selectPlatform(tc.manifests, tc.goos, tc.goarch)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got.Digest != tc.want {
				t.Errorf("digest = %q, want %q", got.Digest, tc.want)
			}
		})
	}
}

func TestReadWithTTFBMeasuresTheFirstByte(t *testing.T) {
	t.Run("reads the whole body", func(t *testing.T) {
		body := []byte("a manifest worth reading")
		got, ttfb, err := readWithTTFB(bytes.NewReader(body), 1<<20, time.Now())
		if err != nil {
			t.Fatalf("readWithTTFB: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("body = %q, want %q", got, body)
		}
		if ttfb < 0 {
			t.Errorf("ttfb = %v", ttfb)
		}
	})

	t.Run("empty body is not an error", func(t *testing.T) {
		got, _, err := readWithTTFB(bytes.NewReader(nil), 1<<20, time.Now())
		if err != nil {
			t.Fatalf("readWithTTFB: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("body = %q, want empty", got)
		}
	})

	t.Run("honours the limit", func(t *testing.T) {
		got, _, err := readWithTTFB(bytes.NewReader([]byte("0123456789")), 4, time.Now())
		if err != nil {
			t.Fatalf("readWithTTFB: %v", err)
		}
		if len(got) != 4 {
			t.Errorf("read %d bytes, want 4", len(got))
		}
	})

	t.Run("a broken body surfaces", func(t *testing.T) {
		broken := io.MultiReader(bytes.NewReader([]byte("x")), errReader{})
		if _, _, err := readWithTTFB(broken, 1<<20, time.Now()); err == nil {
			t.Fatal("want an error from a broken body")
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:           "0 B",
		512:         "512 B",
		1024:        "1.0 KiB",
		1536:        "1.5 KiB",
		1 << 20:     "1.0 MiB",
		3 << 30:     "3.0 GiB",
		8 << 20:     "8.0 MiB",
		5 * 1 << 20: "5.0 MiB",
	}

	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	cases := map[string]struct {
		header string
		want   string
	}{
		"seconds":     {"30", "rate limited; retry after 30s"},
		"http date":   {"Wed, 21 Oct 2026 07:28:00 GMT", "rate limited; retry after Wed, 21 Oct 2026 07:28:00 GMT"},
		"not present": {"", "rate limited"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tc.header != "" {
				resp.Header.Set("Retry-After", tc.header)
			}
			if got := retryAfter(resp); got != tc.want {
				t.Errorf("retryAfter = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseManifestIgnoresNonsense(t *testing.T) {
	if doc := parseManifest([]byte("{ not json")); len(doc.Layers) != 0 || len(doc.Manifests) != 0 {
		t.Errorf("want an empty document, got %+v", doc)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func (l *requestLog) sawPath(prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range l.paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
