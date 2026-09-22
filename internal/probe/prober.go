package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/DC1024/mirrorpilot/internal/mirror"
)

// Defaults for the engine. The per-request budget is deliberately short: a
// mirror that cannot answer in fifteen seconds is not one anybody should be
// configuring, and a long timeout turns a single stuck host into a slow page.
const (
	DefaultConnectTimeout = 5 * time.Second
	DefaultTimeout        = 15 * time.Second

	// DefaultMaxBlobBytes caps layer 4. Enough to get past TCP slow start and
	// produce a number that means something, small enough that probing a dozen
	// mirrors is not a bandwidth event.
	DefaultMaxBlobBytes = 8 << 20

	DefaultUserAgent = "MirrorPilot/1.0 (+https://github.com/DC1024/mirrorpilot)"

	// maxManifestBytes bounds a manifest read. Real ones are tens of kilobytes;
	// this is a guard against a broken or hostile endpoint streaming forever.
	maxManifestBytes = 4 << 20
)

// manifestAccept lists every manifest media type we can act on. Registries are
// within their rights to ignore it and return something else, which the digest
// check then catches.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Options configures a Prober. The zero value is not usable; call New.
type Options struct {
	// Target is the image probed at layers 3 and 4.
	Target Target

	// ConnectTimeout bounds dialling and the TLS handshake.
	ConnectTimeout time.Duration

	// Timeout bounds one whole probe of one mirror.
	Timeout time.Duration

	// MaxBlobBytes caps how much of the layer-4 blob is downloaded. A cap means
	// the blob's digest cannot be checked, which the Result reports honestly
	// rather than treating as a mismatch.
	MaxBlobBytes int64

	// UserAgent identifies this panel to the mirrors it measures. Being
	// identifiable is a courtesy to whoever pays for the bandwidth.
	UserAgent string

	// Client overrides the HTTP client, which tests use to reach an httptest
	// server. Production leaves it nil.
	Client *http.Client
}

// Prober measures mirrors. It is safe for concurrent use.
type Prober struct {
	opts   Options
	client *http.Client
}

// New builds a Prober, applying defaults for anything left unset.
func New(opts Options) (*Prober, error) {
	if err := opts.Target.validate(); err != nil {
		return nil, err
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = DefaultConnectTimeout
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxBlobBytes <= 0 {
		opts.MaxBlobBytes = DefaultMaxBlobBytes
	}
	if opts.UserAgent == "" {
		opts.UserAgent = DefaultUserAgent
	}

	client := opts.Client
	if client == nil {
		client = defaultClient(opts.ConnectTimeout)
	}

	return &Prober{opts: opts, client: client}, nil
}

// defaultClient builds the transport probes use.
//
// Proxy is set explicitly to ProxyFromEnvironment, which is also Go's zero
// behaviour, so a measurement follows the same path this host would really use.
// A probed number that does not match what docker pull will experience is worse
// than no number at all.
func defaultClient(connectTimeout time.Duration) *http.Client {
	return &http.Client{
		// The per-probe context carries the deadline; a client-level timeout
		// would also cap the blob read in a way that lies about throughput.
		Timeout: 0,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   connectTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   connectTimeout,
			ExpectContinueTimeout: time.Second,
			// A few spare connections, so the layers of one probe reuse a
			// connection instead of handshaking four times, which would make
			// the timings meaningless.
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
}

// Target returns the image this Prober measures.
func (p *Prober) Target() Target { return p.opts.Target }

// Probe runs all four layers against one mirror.
//
// It does not return an error: a mirror misbehaving is data, not a failure of
// the caller, and the Result carries whatever went wrong. Only a malformed
// mirror URL short-circuits the run.
func (p *Prober) Probe(ctx context.Context, src mirror.Source) Result {
	res := Result{SourceID: src.ID, StartedAt: time.Now().UTC()}

	base := strings.TrimRight(src.URL, "/")
	if base == "" {
		res.Connect = Layer{Status: StatusFailed, Detail: "mirror has no URL"}
		res.Status, res.Detail = StatusFailed, "mirror has no URL"
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	connect, challenge := p.checkConnectivity(ctx, base)
	res.Connect = connect
	if !connect.Status.Alive() {
		return verdict(res)
	}
	// Being told to back off and pressing on anyway is how a client earns a
	// ban. The remaining layers are skipped, and the verdict still reports the
	// mirror as alive-but-throttling.
	if connect.Status == StatusRateLimited {
		return verdict(res)
	}

	token, tokenLayer := p.fetchToken(ctx, src, challenge)
	res.Token = tokenLayer
	if tokenLayer.Status == StatusRateLimited {
		return verdict(res)
	}

	// A mirror that answers a challenge mid-flight (typically because it
	// redirected us somewhere and the redirect stripped our credentials)
	// deserves the same treatment a real client gives it: read the new
	// challenge, fetch a token for it, and try once more.
	reauth := func(ch authChallenge) (string, bool) {
		tok, layer := p.fetchToken(ctx, src, ch)
		return tok, layer.Status == StatusOK
	}

	body, digest, manifestLayer := p.fetchManifest(ctx, base, token, p.opts.Target.Reference, reauth)
	res.Manifest = manifestLayer
	res.ResolvedDigest = digest
	if manifestLayer.Status != StatusOK {
		return verdict(res)
	}

	throughput, read, checked, verified := p.fetchBlob(ctx, base, token, parseManifest(body), reauth)
	res.Throughput = throughput
	res.Bytes = read
	res.BlobDigestChecked = checked
	res.BlobDigestOK = verified

	return verdict(res)
}

// verdict derives the overall status, in the order the questions actually
// matter: is it answering at all, is it throttling us, can we read a manifest,
// and only then how fast is it.
func verdict(res Result) Result {
	if !res.Connect.Status.Alive() {
		res.Status = res.Connect.Status
		res.Detail = res.Connect.Detail
		return res
	}

	// Throttling outranks everything below it: a 429 tells us nothing about
	// whether the mirror works, only that it will not talk to us right now.
	for _, l := range []Layer{res.Connect, res.Token, res.Manifest, res.Throughput} {
		if l.Status == StatusRateLimited {
			res.Status = StatusRateLimited
			res.Detail = firstNonEmpty(l.Detail, "mirror is rate limiting this client")
			return res
		}
	}

	if res.Manifest.Status != StatusOK {
		res.Status = res.Manifest.Status
		res.Detail = res.Manifest.Detail
		return res
	}

	// A negative digest check only counts when the check could actually be
	// made. A deliberately capped read is not a mirror lying about content.
	if res.BlobDigestChecked && !res.BlobDigestOK {
		res.Status = StatusFailed
		res.Detail = "blob content did not match the digest its manifest promised"
		return res
	}

	if res.Throughput.Status != StatusOK {
		res.Status = res.Throughput.Status
		res.Detail = firstNonEmpty(res.Throughput.Detail, "no blob could be read")
		return res
	}

	res.Status = StatusOK
	if res.Token.Status == StatusSkipped {
		res.Detail = "measured without requesting a token"
	}
	return res
}

// checkConnectivity is layer 1. A 401 or 403 is alive — the host is up and
// speaking the protocol — and its WWW-Authenticate header, when present, is
// carried out as the challenge the token layer will follow.
func (p *Prober) checkConnectivity(ctx context.Context, base string) (Layer, authChallenge) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return Layer{Status: StatusFailed, Detail: "malformed mirror URL"}, authChallenge{}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.opts.UserAgent)

	resp, err := p.client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return Layer{Status: StatusUnreachable, Duration: elapsed, Detail: reason(err)}, authChallenge{}
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		return Layer{Status: StatusOK, Duration: elapsed}, authChallenge{}
	case http.StatusUnauthorized, http.StatusForbidden:
		// Still alive: the host is up and speaking the protocol, which is all
		// layer 1 asks. Whether we may pull from it is layer 3's problem.
		//
		// The challenge decides what this answer *means*. A Bearer realm is
		// how a registry tells an anonymous client where to get a token;
		// every anonymous docker pull starts with exactly this 401, and
		// docker follows it rather than asking anybody to log in. Calling
		// that "needs credentials" is how the page ended up describing
		// working mirrors as locked. With no realm there is nothing to
		// follow, and then a refusal really is a refusal.
		ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
		if ch.realm == "" {
			return Layer{Status: StatusUnauthorized, Duration: elapsed,
				Detail: "refused anonymous access without advertising a token realm"}, authChallenge{}
		}
		return Layer{Status: StatusOK, Duration: elapsed, Detail: challengeNote(ch)}, ch
	case http.StatusTooManyRequests:
		return Layer{Status: StatusRateLimited, Duration: elapsed, Detail: retryAfter(resp)}, authChallenge{}
	default:
		return Layer{Status: StatusFailed, Duration: elapsed,
			Detail: fmt.Sprintf("HTTP %d from /v2/", resp.StatusCode)}, authChallenge{}
	}
}

// authChallenge is what a registry's 401 advertises about where tokens come
// from. The zero value means no challenge was advertised.
type authChallenge struct {
	realm   string
	service string
}

// parseChallenge reads the Bearer parameters out of a WWW-Authenticate value.
//
// The header is a challenge per RFC 6750: a scheme plus comma-separated
// key=value pairs, values optionally quoted. Only what the token request needs
// is kept — the realm to ask and the service to name there — and anything
// unreadable yields the zero value, which the caller treats as "no challenge".
// A quoted value containing a comma would be split wrongly; real challenges do
// not put commas in realms, and a misparse here degrades to probing without a
// token, which the manifest layer reports honestly if it then fails.
func parseChallenge(header string) authChallenge {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return authChallenge{}
	}

	var ch authChallenge
	for _, param := range strings.Split(header[len(scheme):], ",") {
		key, value, ok := strings.Cut(param, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "realm":
			ch.realm = value
		case "service":
			ch.service = value
		}
	}
	return ch
}

// challengeNote explains a challenge in the words a reader needs: who answered,
// and where the anonymous token therefore has to come from.
func challengeNote(ch authChallenge) string {
	if host := realmHost(ch.realm); host != "" {
		return "anonymous pulls need a token from " + host
	}
	return "anonymous pulls need a token"
}

// realmHost is the authority a realm points at, or empty when the realm cannot
// be parsed. It only ever feeds an explanation, so it does not need to decide
// whether the realm is usable — fetchToken does that.
func realmHost(realm string) string {
	u, err := url.Parse(realm)
	if err != nil {
		return ""
	}
	return u.Host
}

// fetchToken is layer 2.
//
// The endpoint comes from the registry, not from us: the challenge on the 401
// names the realm that issues tokens, and that realm frequently lives
// somewhere other than the mirror itself — DaoCloud's sits on m.daocloud.io
// while the mirror is docker.m.daocloud.io, and a proxy may point at upstream's
// auth altogether. Asking the mirror for /token because it seems like the
// natural place is how a mirror that answers anonymous pulls all day gets
// reported as needing credentials.
//
// No challenge, no token request: a registry that answered 200 on /v2/ asked
// for nothing, and inventing a token endpoint it never mentioned is guessing.
// Plenty of registries serve anonymous pulls without one.
func (p *Prober) fetchToken(ctx context.Context, src mirror.Source, challenge authChallenge) (string, Layer) {
	if challenge.realm == "" {
		return "", Layer{Status: StatusSkipped, Detail: "registry did not request a token"}
	}
	if u, err := url.Parse(challenge.realm); err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return "", Layer{Status: StatusSkipped, Detail: "token realm was not a usable URL"}
	}

	query := url.Values{}
	query.Set("service", firstNonEmpty(challenge.service, src.Host()))
	query.Set("scope", "repository:"+p.opts.Target.Repository+":pull")

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, challenge.realm+"?"+query.Encode(), nil)
	if err != nil {
		return "", Layer{Status: StatusFailed, Detail: "malformed token URL"}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", p.opts.UserAgent)

	resp, err := p.client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return "", Layer{Status: StatusUnreachable, Duration: elapsed, Detail: reason(err)}
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		var payload struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
		if err != nil {
			return "", Layer{Status: StatusFailed, Duration: elapsed, Detail: "token response could not be read"}
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", Layer{Status: StatusFailed, Duration: elapsed, Detail: "token response was not JSON"}
		}
		token := firstNonEmpty(payload.Token, payload.AccessToken)
		if token == "" {
			return "", Layer{Status: StatusFailed, Duration: elapsed, Detail: "token response contained no token"}
		}
		return token, Layer{Status: StatusOK, Duration: elapsed}
	case http.StatusNotFound, http.StatusNotImplemented, http.StatusMethodNotAllowed:
		return "", Layer{Status: StatusSkipped, Duration: elapsed, Detail: "no token endpoint"}
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", Layer{Status: StatusUnauthorized, Duration: elapsed, Detail: "token request was refused"}
	case http.StatusTooManyRequests:
		return "", Layer{Status: StatusRateLimited, Duration: elapsed, Detail: retryAfter(resp)}
	default:
		return "", Layer{Status: StatusFailed, Duration: elapsed,
			Detail: fmt.Sprintf("HTTP %d from /token", resp.StatusCode)}
	}
}

// reauthFunc fetches a token for a challenge that arrived mid-probe. It
// reports whether a usable token came back, so the caller can decide whether
// a retry is worth making.
type reauthFunc func(authChallenge) (string, bool)

// fetchManifest is layer 3. It returns the manifest body and its digest.
//
// The digest is computed from the bytes we received, never read from the
// Docker-Content-Digest header: a mirror that rewrites content can rewrite the
// header to match, so the only digest worth trusting is the one we calculate.
// The header is still consulted, because a disagreement between the two is
// worth reporting.
func (p *Prober) fetchManifest(ctx context.Context, base, token, reference string, reauth reauthFunc) ([]byte, string, Layer) {
	target := base + "/v2/" + p.opts.Target.Repository + "/manifests/" + reference
	u, err := url.Parse(target)
	if err != nil {
		return nil, "", Layer{Status: StatusFailed, Detail: "malformed mirror URL"}
	}
	return p.fetchManifestURL(ctx, u, token, reauth)
}

// fetchManifestURL performs one manifest request, and — when the response is a
// fresh challenge, which is what a redirect across hosts leaves behind after
// stripping the Authorization header — retries once against the final URL with
// a token minted for that challenge. That is the same courtesy a real client
// extends, and without it an alias that redirects to an authenticated registry
// reads as "needs credentials" although a plain docker pull succeeds.
func (p *Prober) fetchManifestURL(ctx context.Context, u *url.URL, token string, reauth reauthFunc) ([]byte, string, Layer) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", Layer{Status: StatusFailed, Detail: "malformed mirror URL"}
	}
	req.Header.Set("Accept", manifestAccept)
	req.Header.Set("User-Agent", p.opts.UserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", Layer{Status: StatusUnreachable,
			Duration: time.Since(start), Detail: reason(err)}
	}
	defer closeBody(resp)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if ch := parseChallenge(resp.Header.Get("WWW-Authenticate")); ch.realm != "" && reauth != nil {
			if tok2, ok := reauth(ch); ok {
				// One retry, against the URL we actually landed on. The nil
				// keeps a mirror that challenges forever from looping us.
				return p.fetchManifestURL(ctx, resp.Request.URL, tok2, nil)
			}
		}
	}

	if failure, ok := manifestHTTPFailure(resp, start); ok {
		return nil, "", failure
	}

	body, elapsed, err := readWithTTFB(resp.Body, maxManifestBytes, start)
	if err != nil {
		return nil, "", Layer{Status: StatusFailed, Duration: elapsed, Detail: "manifest body could not be read"}
	}

	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	layer := Layer{Status: StatusOK, Duration: elapsed}
	if declared := resp.Header.Get("Docker-Content-Digest"); declared != "" && declared != digest {
		layer.Status = StatusFailed
		layer.Detail = "mirror declared one manifest digest and served another"
	}

	return body, digest, layer
}

// manifestHTTPFailure maps a non-200 manifest response onto a layer status.
func manifestHTTPFailure(resp *http.Response, start time.Time) (Layer, bool) {
	elapsed := time.Since(start)

	switch resp.StatusCode {
	case http.StatusOK:
		return Layer{}, false
	case http.StatusUnauthorized, http.StatusForbidden:
		return Layer{Status: StatusUnauthorized, Duration: elapsed,
			Detail: "manifest requires credentials"}, true
	case http.StatusTooManyRequests:
		return Layer{Status: StatusRateLimited, Duration: elapsed, Detail: retryAfter(resp)}, true
	default:
		return Layer{Status: StatusFailed, Duration: elapsed,
			Detail: fmt.Sprintf("HTTP %d for the manifest", resp.StatusCode)}, true
	}
}

// fetchBlob is layer 4, and returns the bytes read plus whether the content
// hashed to the digest the manifest promised.
//
// The second boolean reports whether a digest check was possible at all: a read
// capped by MaxBlobBytes cannot be verified, and saying "unverified" is not the
// same as saying "wrong".
func (p *Prober) fetchBlob(ctx context.Context, base, token string, doc manifestDoc, reauth reauthFunc) (Layer, int64, bool, bool) {
	blob, ok := p.pickBlob(ctx, base, token, doc, reauth)
	if !ok {
		return Layer{Status: StatusSkipped, Detail: "manifest listed no blob for this platform"}, 0, false, false
	}
	if blob.Digest == "" {
		return Layer{Status: StatusFailed, Detail: "manifest listed a blob with no digest"}, 0, false, false
	}

	target := base + "/v2/" + p.opts.Target.Repository + "/blobs/" + blob.Digest
	u, err := url.Parse(target)
	if err != nil {
		return Layer{Status: StatusFailed, Detail: "malformed mirror URL"}, 0, false, false
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Layer{Status: StatusFailed, Detail: "malformed mirror URL"}, 0, false, false
	}
	req.Header.Set("User-Agent", p.opts.UserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return Layer{Status: StatusUnreachable, Duration: time.Since(start),
			Detail: reason(err)}, 0, false, false
	}
	defer closeBody(resp)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Same story as the manifest: a redirect across hosts stripped the
		// credentials, and the challenge names where a new token lives.
		if ch := parseChallenge(resp.Header.Get("WWW-Authenticate")); ch.realm != "" && reauth != nil {
			if tok2, ok := reauth(ch); ok {
				return p.fetchBlobAt(ctx, resp.Request.URL, tok2, blob)
			}
		}
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return Layer{Status: StatusUnauthorized, Duration: time.Since(start),
			Detail: "blob requires credentials"}, 0, false, false
	case http.StatusTooManyRequests:
		return Layer{Status: StatusRateLimited, Duration: time.Since(start),
			Detail: retryAfter(resp)}, 0, false, false
	default:
		return Layer{Status: StatusFailed, Duration: time.Since(start),
			Detail: fmt.Sprintf("HTTP %d for the blob", resp.StatusCode)}, 0, false, false
	}

	hasher := sha256.New()
	read, err := io.Copy(hasher, io.LimitReader(resp.Body, p.opts.MaxBlobBytes))
	elapsed := time.Since(start)

	layer := Layer{Status: StatusOK, Duration: elapsed}
	if err != nil {
		layer = Layer{Status: StatusFailed, Duration: elapsed, Detail: interruptedDetail(read)}
	}

	complete := read >= blob.Size
	if !complete && layer.Status == StatusOK {
		layer.Detail = fmt.Sprintf("read capped at %s of %s", HumanBytes(read), HumanBytes(blob.Size))
	}

	verified := complete && "sha256:"+hex.EncodeToString(hasher.Sum(nil)) == blob.Digest
	return layer, read, complete, verified
}

// fetchBlobAt reads the blob at an absolute URL — the retry half of fetchBlob,
// after a mid-flight challenge produced a fresh token. One attempt, no
// further retries: a mirror that challenges forever does not get a loop.
// The verification contract is the same as the main path's: the bytes are
// hashed, and only a complete read that matches the manifest's digest counts.
func (p *Prober) fetchBlobAt(ctx context.Context, u *url.URL, token string, blob descriptor) (Layer, int64, bool, bool) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Layer{Status: StatusFailed, Detail: "malformed mirror URL"}, 0, false, false
	}
	req.Header.Set("User-Agent", p.opts.UserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return Layer{Status: StatusUnreachable, Duration: time.Since(start),
			Detail: reason(err)}, 0, false, false
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return Layer{Status: StatusRateLimited, Duration: time.Since(start),
				Detail: retryAfter(resp)}, 0, false, false
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return Layer{Status: StatusUnauthorized, Duration: time.Since(start),
				Detail: "blob requires credentials"}, 0, false, false
		default:
			return Layer{Status: StatusFailed, Duration: time.Since(start),
				Detail: fmt.Sprintf("HTTP %d for the blob", resp.StatusCode)}, 0, false, false
		}
	}

	hasher := sha256.New()
	read, err := io.Copy(hasher, io.LimitReader(resp.Body, p.opts.MaxBlobBytes))
	elapsed := time.Since(start)

	layer := Layer{Status: StatusOK, Duration: elapsed}
	if err != nil {
		layer = Layer{Status: StatusFailed, Duration: elapsed, Detail: interruptedDetail(read)}
	}

	complete := read >= blob.Size
	if !complete && layer.Status == StatusOK {
		layer.Detail = fmt.Sprintf("read capped at %s of %s", HumanBytes(read), HumanBytes(blob.Size))
	}

	verified := complete && "sha256:"+hex.EncodeToString(hasher.Sum(nil)) == blob.Digest
	return layer, read, complete, verified
}

// pickBlob resolves a manifest down to the one blob worth measuring.
//
// A multi-platform index first has to be followed to the manifest for this
// host's architecture, because probing another architecture produces a number
// for an image this machine can never run.
func (p *Prober) pickBlob(ctx context.Context, base, token string, doc manifestDoc, reauth reauthFunc) (descriptor, bool) {
	if len(doc.Manifests) > 0 {
		chosen, ok := selectPlatform(doc.Manifests, runtime.GOOS, runtime.GOARCH)
		if !ok {
			return descriptor{}, false
		}

		// Re-fetch by digest so the child is pinned to the exact bytes the
		// index named, rather than to whatever a tag means now.
		body, _, layer := p.fetchManifest(ctx, base, token, chosen.Digest, reauth)
		if layer.Status != StatusOK {
			return descriptor{}, false
		}
		doc = parseManifest(body)
	}

	if len(doc.Layers) == 0 {
		return descriptor{}, false
	}

	// Measure the largest layer: it gives the transfer long enough to leave TCP
	// slow start behind, so the result reflects sustained speed rather than
	// handshake overhead.
	best := doc.Layers[0]
	for _, l := range doc.Layers[1:] {
		if l.Size > best.Size {
			best = l
		}
	}
	return best, true
}

func parseManifest(body []byte) manifestDoc {
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return manifestDoc{}
	}
	return doc
}

// selectPlatform picks the manifest for one platform, preferring a
// variant-less entry and falling back to the first variant that matches.
func selectPlatform(manifests []descriptor, goos, goarch string) (descriptor, bool) {
	var fallback descriptor
	var haveFallback bool

	for _, m := range manifests {
		if m.Platform == nil || m.Platform.OS != goos || m.Platform.Architecture != goarch {
			continue
		}
		if m.Platform.Variant == "" {
			return m, true
		}
		if !haveFallback {
			fallback, haveFallback = m, true
		}
	}
	return fallback, haveFallback
}

// readWithTTFB reads a body and reports the time to its first byte rather than
// to its last one, which is what the third layer is supposed to measure.
func readWithTTFB(body io.Reader, limit int64, start time.Time) ([]byte, time.Duration, error) {
	var buf bytes.Buffer

	first := make([]byte, 1)
	n, err := io.ReadFull(body, first)
	ttfb := time.Since(start)
	buf.Write(first[:n])

	switch {
	case err == nil:
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// Short body. Still a valid measurement of when the first byte landed.
		return buf.Bytes(), ttfb, nil
	default:
		return nil, ttfb, err
	}

	if remaining := limit - int64(n); remaining > 0 {
		rest, err := io.ReadAll(io.LimitReader(body, remaining))
		if err != nil {
			return nil, ttfb, err
		}
		buf.Write(rest)
	}

	return buf.Bytes(), ttfb, nil
}

func closeBody(resp *http.Response) {
	// Drain a little so the connection can be reused. Blob bodies are normally
	// fully read already; this is for the rest.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// reason reduces a transport error to something a user can act on. The full
// error embeds the URL, which is noise in a UI that already shows it.
func reason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return err.Error()
}

func retryAfter(resp *http.Response) string {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return fmt.Sprintf("rate limited; retry after %ds", secs)
		}
		return "rate limited; retry after " + v
	}
	return "rate limited"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// HumanBytes renders a byte count for a message a person will read.
//
// Exported because the panel renders the same quantities the engine measures.
// Two implementations of this would drift, and the one that drifted would be
// the one nobody was looking at.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

// interruptedDetail words a broken blob read.
//
// Where the transfer stopped is the whole diagnosis, and it is the one fact
// the counters can still supply after the connection is gone. No bytes at all
// means the mirror answered the request and then sent nothing — a different
// fault from one that streams for a while and gives up, and the two should
// not share a sentence that turns the second into a rounded-down zero.
func interruptedDetail(read int64) string {
	if read <= 0 {
		return "blob transfer was interrupted before any data arrived"
	}
	return fmt.Sprintf("blob transfer was interrupted after %s", HumanBytes(read))
}

// descriptor is one entry in an index, or one blob in a manifest.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

// manifestDoc covers both an image index and a single image manifest. Only one
// of Manifests or Layers is populated, which is how the OCI types are told
// apart in practice.
type manifestDoc struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
}
