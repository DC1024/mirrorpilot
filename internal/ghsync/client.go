package ghsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the public GitHub API. Tests point the client elsewhere.
const DefaultBaseURL = "https://api.github.com"

// apiVersion pins the response shape. GitHub keeps older versions working, so
// pinning is what stops a future default from changing what this code parses.
const apiVersion = "2022-11-28"

// defaultTimeout bounds one request. Dispatch is a single fast call; the run
// and job listings are small. A request that hangs is a page that hangs.
const defaultTimeout = 20 * time.Second

// maxResponseBytes caps what is read from a response. Every response this
// client wants is a few kilobytes, and reading an unbounded body from a
// remote service into memory is a way to lose a container.
const maxResponseBytes = 1 << 20

// Errors a caller is expected to distinguish. Every failure is wrapped in one
// of these, with GitHub's own message appended: the message is usually the
// whole diagnosis ("Workflow does not have 'workflow_dispatch' trigger"), and
// a generic "request failed" would throw it away.
var (
	ErrUnauthorized = errors.New("ghsync: the token was rejected")
	ErrForbidden    = errors.New("ghsync: the token is not allowed to do that")
	ErrNotFound     = errors.New("ghsync: not found")
	ErrRateLimited  = errors.New("ghsync: GitHub rate limit reached")
	ErrRejected     = errors.New("ghsync: GitHub rejected the request")
)

// Client talks to the GitHub REST API.
type Client struct {
	token string
	base  *url.URL
	http  *http.Client
}

// Option adjusts a Client.
type Option func(*Client)

// WithBaseURL points the client at a different API root, for tests.
func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			c.base = parsed
		}
	}
}

// WithHTTPClient replaces the HTTP client, for tests and for a caller that
// wants its own transport.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient != nil {
			c.http = httpClient
		}
	}
}

// NewClient builds a client for a token.
//
// An empty token is rejected rather than allowed through: every endpoint this
// client uses needs authentication, and a client that fails at the first
// request with a 401 is a worse experience than one that refuses to exist.
func NewClient(token string, opts ...Option) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("%w (no token was given)", ErrUnauthorized)
	}

	c := &Client{
		token: token,
		http:  &http.Client{Timeout: defaultTimeout},
	}
	if parsed, err := url.Parse(DefaultBaseURL); err == nil {
		c.base = parsed
	}

	for _, opt := range opts {
		opt(c)
	}
	if c.base == nil {
		return nil, errors.New("ghsync: no API base URL")
	}

	return c, nil
}

// User returns the account the token belongs to.
//
// It is the cheapest way to answer "is this token any good", and it names the
// account so a mistyped token for the wrong user is visible before anything is
// dispatched.
func (c *Client) User(ctx context.Context) (User, error) {
	var payload struct {
		Login   string `json:"login"`
		Name    string `json:"name"`
		HTMLURL string `json:"html_url"`
	}

	if err := c.do(ctx, http.MethodGet, "/user", nil, &payload); err != nil {
		return User{}, err
	}

	return User{Login: payload.Login, Name: payload.Name, URL: payload.HTMLURL}, nil
}

// Dispatch asks GitHub to run the workflow.
//
// A success is a 204 with no body: the API accepts the request and says
// nothing else. Whether the copy works is a separate question, answered by
// polling the run.
func (c *Client) Dispatch(ctx context.Context, spec Spec, in Inputs) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}

	body := map[string]any{"inputs": in.Encode()}
	if spec.Ref != "" {
		body["ref"] = spec.Ref
	}

	return c.do(ctx, http.MethodPost, dispatchesPath(spec), body, nil)
}

// Runs lists a workflow's recent runs, newest first as GitHub returns them.
func (c *Client) Runs(ctx context.Context, spec Spec, perPage int) ([]Run, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if perPage <= 0 {
		perPage = 5
	}

	var payload struct {
		WorkflowRuns []apiRun `json:"workflow_runs"`
	}

	path := fmt.Sprintf("%s?per_page=%d", runsPath(spec), perPage)
	if err := c.do(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return nil, err
	}

	out := make([]Run, 0, len(payload.WorkflowRuns))
	for _, run := range payload.WorkflowRuns {
		out = append(out, run.toRun())
	}
	return out, nil
}

// LatestRun returns the newest run, or ErrNotFound when the workflow has never
// run. That is a normal state for a freshly copied workflow file, so callers
// are expected to treat it as "nothing yet".
func (c *Client) LatestRun(ctx context.Context, spec Spec) (Run, error) {
	runs, err := c.Runs(ctx, spec, 1)
	if err != nil {
		return Run{}, err
	}
	if len(runs) == 0 {
		return Run{}, fmt.Errorf("%w: %s has no runs yet", ErrNotFound, spec.Workflow)
	}
	return runs[0], nil
}

// Run returns one run by id.
func (c *Client) Run(ctx context.Context, spec Spec, id int64) (Run, error) {
	if err := spec.Validate(); err != nil {
		return Run{}, err
	}
	if id <= 0 {
		return Run{}, fmt.Errorf("%w: run id %d is not a run id", ErrInvalid, id)
	}

	var payload apiRun
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", spec.Owner, spec.Repo, id)
	if err := c.do(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return Run{}, err
	}
	return payload.toRun(), nil
}

// Jobs lists the jobs inside a run.
//
// This is where a failed relocation explains itself: "the pull step failed" is
// actionable in a way that "the run failed" is not.
func (c *Client) Jobs(ctx context.Context, spec Spec, runID int64) ([]Job, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if runID <= 0 {
		return nil, fmt.Errorf("%w: run id %d is not a run id", ErrInvalid, runID)
	}

	var payload struct {
		Jobs []apiJob `json:"jobs"`
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=50", spec.Owner, spec.Repo, runID)
	if err := c.do(ctx, http.MethodGet, path, nil, &payload); err != nil {
		return nil, err
	}

	out := make([]Job, 0, len(payload.Jobs))
	for _, job := range payload.Jobs {
		out = append(out, job.toJob())
	}
	return out, nil
}

// apiRun is a run as GitHub spells it.
type apiRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Event      string `json:"event"`
	Name       string `json:"name"`
	HTMLURL    string `json:"html_url"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func (a apiRun) toRun() Run {
	return Run{
		ID:         a.ID,
		Status:     a.Status,
		Conclusion: a.Conclusion,
		Event:      a.Event,
		Title:      a.Name,
		URL:        a.HTMLURL,
		HeadBranch: a.HeadBranch,
		HeadSHA:    a.HeadSHA,
		CreatedAt:  parseTime(a.CreatedAt),
		UpdatedAt:  parseTime(a.UpdatedAt),
	}
}

type apiJob struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	HTMLURL     string `json:"html_url"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
}

func (a apiJob) toJob() Job {
	return Job{
		ID:          a.ID,
		Name:        a.Name,
		Status:      a.Status,
		Conclusion:  a.Conclusion,
		URL:         a.HTMLURL,
		StartedAt:   parseTime(a.StartedAt),
		CompletedAt: parseTime(a.CompletedAt),
	}
}

// parseTime reads GitHub's RFC 3339 timestamps, returning the zero time for
// anything it cannot read.
//
// A timestamp this package cannot parse is worth losing rather than worth
// failing a request over: every caller treats the zero time as "not known",
// and the alternative is a run list that cannot render because one field of
// one run was odd.
func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// do performs one request and decodes the response into out when it is not nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("ghsync: encode request for %s: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	// Joined by hand rather than with ResolveReference, which would drop the
	// base's path: a self-hosted enterprise API lives under a prefix, and a
	// silently truncated prefix is a 404 nobody can explain.
	relPath, query, _ := strings.Cut(path, "?")

	target := *c.base
	target.Path = strings.TrimRight(c.base.Path, "/") + relPath
	target.RawQuery = query

	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return fmt.Errorf("ghsync: build request for %s: %w", path, err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ghsync: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("ghsync: read response for %s: %w", path, err)
	}

	if resp.StatusCode/100 != 2 {
		return classify(resp, payload, method, path)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("ghsync: decode response for %s: %w", path, err)
	}
	return nil
}

// classify turns a non-2xx response into a typed error carrying GitHub's own
// explanation.
func classify(resp *http.Response, payload []byte, method, path string) error {
	detail := apiMessage(payload)
	if detail == "" {
		detail = strings.ToLower(http.StatusText(resp.StatusCode))
	}

	wrap := func(sentinel error) error {
		return fmt.Errorf("%w: %s %s: %s", sentinel, method, path, detail)
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return wrap(ErrUnauthorized)
	case http.StatusForbidden:
		// A spent rate limit answers 403 as well, and it is a completely
		// different instruction to the reader: wait, rather than fix a scope.
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return wrap(ErrRateLimited)
		}
		return wrap(ErrForbidden)
	case http.StatusNotFound:
		return wrap(ErrNotFound)
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		return wrap(ErrRejected)
	default:
		if resp.StatusCode >= 500 {
			return wrap(ErrRejected)
		}
		return wrap(ErrRejected)
	}
}

// apiMessage pulls GitHub's explanation out of an error body.
func apiMessage(payload []byte) string {
	var parsed struct {
		Message string `json:"message"`
		Errors  []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return ""
	}

	message := strings.TrimSpace(parsed.Message)
	for _, item := range parsed.Errors {
		switch {
		case item.Field != "" && item.Code != "":
			message += fmt.Sprintf(" (%s: %s)", item.Field, item.Code)
		case item.Code != "":
			message += fmt.Sprintf(" (%s)", item.Code)
		}
	}
	return message
}

// dispatchesPath is the endpoint that starts a workflow.
func dispatchesPath(spec Spec) string {
	return fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches",
		spec.Owner, spec.Repo, spec.Workflow)
}

// runsPath is the endpoint that lists a workflow's runs.
func runsPath(spec Spec) string {
	return fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs",
		spec.Owner, spec.Repo, spec.Workflow)
}
