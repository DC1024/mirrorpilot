package ghsync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "ghp_example"

// harness is a fake GitHub that records what it was asked.
type harness struct {
	*httptest.Server

	lastAuth     string
	lastVersion  string
	lastQuery    string
	lastDispatch map[string]any
	requests     []string
}

func newHarness(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()

	h := &harness{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.lastAuth = r.Header.Get("Authorization")
		h.lastVersion = r.Header.Get("X-GitHub-Api-Version")
		h.lastQuery = r.URL.RawQuery
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)

		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				h.lastDispatch = body
			}
		}
		handler(w, r)
	}))
	t.Cleanup(h.Close)

	return h
}

func (h *harness) client(t *testing.T) *Client {
	t.Helper()

	client, err := NewClient(testToken, WithBaseURL(h.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func testSpec() Spec {
	return Spec{Owner: "DC1024", Repo: "image-relay", Workflow: DefaultWorkflowFile, Ref: "main"}
}

func TestSpecValidate(t *testing.T) {
	valid := testSpec()
	if err := valid.Validate(); err != nil {
		t.Fatalf("a well-formed spec was rejected: %v", err)
	}

	// An empty ref is allowed: it means "the repository's default branch",
	// which is what GitHub does with an omitted ref.
	noRef := testSpec()
	noRef.Ref = ""
	if err := noRef.Validate(); err != nil {
		t.Errorf("an empty ref should be allowed: %v", err)
	}

	for name, tweak := range map[string]func(*Spec){
		"empty owner":      func(s *Spec) { s.Owner = "" },
		"owner with slash": func(s *Spec) { s.Owner = "a/b" },
		"empty repo":       func(s *Spec) { s.Repo = "" },
		"repo is a dot":    func(s *Spec) { s.Repo = ".hidden" },
		"workflow with /":  func(s *Spec) { s.Workflow = "a/b.yml" },
		"ref with a space": func(s *Spec) { s.Ref = "my branch" },
	} {
		spec := testSpec()
		tweak(&spec)
		if err := spec.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestTargetValidateAndPrefix(t *testing.T) {
	target := Target{Registry: "registry.cn-hangzhou.aliyuncs.com", Namespace: "mirrorpilot"}
	if err := target.Validate(); err != nil {
		t.Fatalf("a well-formed target was rejected: %v", err)
	}
	if got, want := target.Prefix(), "registry.cn-hangzhou.aliyuncs.com/mirrorpilot"; got != want {
		t.Errorf("Prefix() = %q, want %q", got, want)
	}

	for name, target := range map[string]Target{
		"empty registry":     {Namespace: "ns"},
		"uppercase registry": {Registry: "Registry.example", Namespace: "ns"},
		"registry with path": {Registry: "registry.example/ns", Namespace: "ns"},
		"empty namespace":    {Registry: "registry.example"},
		"bad namespace":      {Registry: "registry.example", Namespace: "ns/../etc"},
	} {
		if err := target.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestInputsEncodePutsOneImagePerLine(t *testing.T) {
	in := Inputs{
		Target: "registry.example/ns",
		Images: []string{"docker.io/library/alpine:3.21", "ghcr.io/owner/repo:v1"},
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	encoded := in.Encode()
	want := "docker.io/library/alpine:3.21\nghcr.io/owner/repo:v1"
	if encoded["images"] != want {
		t.Errorf("images = %q, want %q", encoded["images"], want)
	}
	if encoded["target"] != "registry.example/ns" {
		t.Errorf("target = %q", encoded["target"])
	}
}

func TestInputsValidateRejectsAnEmptyRun(t *testing.T) {
	for name, in := range map[string]Inputs{
		"no target":   {Images: []string{"alpine"}},
		"no images":   {Target: "registry.example/ns"},
		"blank image": {Target: "registry.example/ns", Images: []string{" "}},
	} {
		if err := in.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestRunStateCollapsesStatusAndConclusion(t *testing.T) {
	for name, tc := range map[string]struct {
		status     string
		conclusion string
		want       State
	}{
		"queued":                  {"queued", "", StateQueued},
		"requested":               {"requested", "", StateQueued},
		"running":                 {"in_progress", "", StateRunning},
		"success":                 {"completed", "success", StateSucceeded},
		"failure":                 {"completed", "failure", StateFailed},
		"timed out":               {"completed", "timed_out", StateFailed},
		"cancelled":               {"completed", "cancelled", StateCancelled},
		"completed with no words": {"completed", "", StateFailed},
		"nonsense":                {"something_new", "", StateUnknown},
	} {
		got := Run{Status: tc.status, Conclusion: tc.conclusion}.State()
		if got != tc.want {
			t.Errorf("%s: State() = %q, want %q", name, got, tc.want)
		}
	}
}

func TestStateTerminal(t *testing.T) {
	for state, want := range map[State]bool{
		StateQueued:    false,
		StateRunning:   false,
		StateSucceeded: true,
		StateFailed:    true,
		StateCancelled: true,
		StateUnknown:   false,
	} {
		if got := state.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", state, got, want)
		}
	}
}

func TestJobDurationIsZeroUntilItHasFinished(t *testing.T) {
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

	running := Job{StartedAt: start}
	if got := running.Duration(); got != 0 {
		t.Errorf("Duration() = %v, want 0 while still running", got)
	}

	done := Job{StartedAt: start, CompletedAt: start.Add(90 * time.Second)}
	if got := done.Duration(); got != 90*time.Second {
		t.Errorf("Duration() = %v, want 90s", got)
	}
}

func TestNewClientRefusesAnEmptyToken(t *testing.T) {
	if _, err := NewClient("   "); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestClientSendsAuthenticationAndAPIVersion(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"DC1024","name":"DC","html_url":"https://github.com/DC1024"}`))
	})

	user, err := h.client(t).User(context.Background())
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if user.Login != "DC1024" {
		t.Errorf("Login = %q", user.Login)
	}
	if h.lastAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", h.lastAuth)
	}
	if h.lastVersion == "" {
		t.Error("the API version header was not sent; a future default could change what we parse")
	}
}

func TestDispatchSendsTheInputsAndTheRef(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	in := Inputs{Target: "registry.example/ns", Images: []string{"docker.io/library/alpine:3.21"}}
	if err := h.client(t).Dispatch(context.Background(), testSpec(), in); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(h.requests) != 1 || h.requests[0] != "POST /repos/DC1024/image-relay/actions/workflows/"+DefaultWorkflowFile+"/dispatches" {
		t.Fatalf("requests = %v", h.requests)
	}
	if h.lastDispatch["ref"] != "main" {
		t.Errorf("ref = %v, want main", h.lastDispatch["ref"])
	}

	inputs, ok := h.lastDispatch["inputs"].(map[string]any)
	if !ok {
		t.Fatalf("inputs = %#v", h.lastDispatch["inputs"])
	}
	if inputs["target"] != "registry.example/ns" {
		t.Errorf("inputs.target = %v", inputs["target"])
	}
	if inputs["images"] != "docker.io/library/alpine:3.21" {
		t.Errorf("inputs.images = %v", inputs["images"])
	}
}

func TestDispatchOmitsAnEmptyRef(t *testing.T) {
	// Sending ref:"" would ask GitHub to run from a branch with no name.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	spec := testSpec()
	spec.Ref = ""

	in := Inputs{Target: "registry.example/ns", Images: []string{"alpine"}}
	if err := h.client(t).Dispatch(context.Background(), spec, in); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, present := h.lastDispatch["ref"]; present {
		t.Errorf("an empty ref was sent: %#v", h.lastDispatch)
	}
}

func TestDispatchValidatesBeforeItSendsAnything(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a rejected dispatch still reached the network")
	})

	if err := h.client(t).Dispatch(context.Background(), testSpec(), Inputs{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestRunsDecodesAndAsksForTheRightPageSize(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workflow_runs":[
			{"id":42,"status":"completed","conclusion":"success","name":"MirrorPilot relay",
			 "html_url":"https://github.com/DC1024/image-relay/actions/runs/42",
			 "head_branch":"main","head_sha":"abc123",
			 "created_at":"2026-09-21T10:00:00Z","updated_at":"2026-09-21T10:02:00Z"}
		]}`))
	})

	runs, err := h.client(t).Runs(context.Background(), testSpec(), 1)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if h.lastQuery != "per_page=1" {
		t.Errorf("query = %q, want per_page=1", h.lastQuery)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}

	run := runs[0]
	if run.ID != 42 || run.State() != StateSucceeded {
		t.Errorf("run = %+v", run)
	}
	if run.CreatedAt.IsZero() || run.CreatedAt.UTC().Hour() != 10 {
		t.Errorf("created_at was not parsed: %v", run.CreatedAt)
	}
	if run.HeadSHA != "abc123" {
		t.Errorf("head_sha = %q", run.HeadSHA)
	}
}

func TestLatestRunReportsNotFoundWhenNothingHasRunYet(t *testing.T) {
	// A freshly committed workflow has no runs, and that is not a failure.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	})

	if _, err := h.client(t).LatestRun(context.Background(), testSpec()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestJobsDecodesTheRunBreakdown(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[
			{"id":7,"name":"relocate","status":"completed","conclusion":"failure",
			 "html_url":"https://github.com/x/y/actions/runs/42/job/7",
			 "started_at":"2026-09-21T10:00:10Z","completed_at":"2026-09-21T10:01:40Z"}
		]}`))
	})

	jobs, err := h.client(t).Jobs(context.Background(), testSpec(), 42)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	if got := jobs[0].State(); got != StateFailed {
		t.Errorf("State() = %q, want failed", got)
	}
	if got := jobs[0].Duration(); got != 90*time.Second {
		t.Errorf("Duration() = %v, want 90s", got)
	}
}

func TestJobTimestampsSurviveBeingMissing(t *testing.T) {
	// GitHub sends null for a job that has not started. Rendering "0001-01-01"
	// would be worse than rendering nothing, and the caller checks IsZero.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[{"id":1,"name":"waiting","status":"queued","conclusion":""}]}`))
	})

	jobs, err := h.client(t).Jobs(context.Background(), testSpec(), 1)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if !jobs[0].StartedAt.IsZero() {
		t.Errorf("StartedAt = %v, want the zero time", jobs[0].StartedAt)
	}
	if got := jobs[0].State(); got != StateQueued {
		t.Errorf("State() = %q, want queued", got)
	}
}

func TestRunRejectsANonsenseID(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a nonsense run id still reached the network")
	})

	if _, err := h.client(t).Run(context.Background(), testSpec(), 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestErrorsCarryGitHubsOwnExplanation(t *testing.T) {
	for name, tc := range map[string]struct {
		status     int
		header     map[string]string
		body       string
		want       error
		wantDetail string
	}{
		"unauthorized": {
			status:     http.StatusUnauthorized,
			body:       `{"message":"Bad credentials"}`,
			want:       ErrUnauthorized,
			wantDetail: "Bad credentials",
		},
		"forbidden": {
			status:     http.StatusForbidden,
			body:       `{"message":"Resource not accessible by personal access token"}`,
			want:       ErrForbidden,
			wantDetail: "not accessible",
		},
		"rate limited": {
			status:     http.StatusForbidden,
			header:     map[string]string{"X-RateLimit-Remaining": "0"},
			body:       `{"message":"API rate limit exceeded"}`,
			want:       ErrRateLimited,
			wantDetail: "rate limit",
		},
		"not found": {
			status:     http.StatusNotFound,
			body:       `{"message":"Not Found"}`,
			want:       ErrNotFound,
			wantDetail: "Not Found",
		},
		"unprocessable": {
			status:     http.StatusUnprocessableEntity,
			body:       `{"message":"Workflow does not have 'workflow_dispatch' trigger"}`,
			want:       ErrRejected,
			wantDetail: "workflow_dispatch",
		},
	} {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			for key, value := range tc.header {
				w.Header().Set(key, value)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})

		_, err := h.client(t).User(context.Background())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
			continue
		}
		// The message is the diagnosis. Throwing it away would leave a reader
		// with "the request failed" and nowhere to go.
		if !strings.Contains(err.Error(), tc.wantDetail) {
			t.Errorf("%s: %v lost GitHub's explanation %q", name, err, tc.wantDetail)
		}
	}
}

func TestErrorIncludesFiledDetails(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"field":"ref","code":"invalid"}]}`))
	})

	_, err := h.client(t).User(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ref: invalid") {
		t.Errorf("err = %v, want the field detail", err)
	}
}

func TestBaseURLPathPrefixIsPreserved(t *testing.T) {
	// GitHub Enterprise serves the API under /api/v3. Dropping the prefix
	// would produce a 404 from a configured, correct-looking address.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user" {
			t.Errorf("path = %q, want /api/v3/user", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"login":"someone"}`))
	})

	client, err := NewClient(testToken, WithBaseURL(h.URL+"/api/v3"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.User(context.Background()); err != nil {
		t.Fatalf("User: %v", err)
	}
}

func TestRunFetchesOneRunByID(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/DC1024/image-relay/actions/runs/42" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":42,"status":"in_progress","conclusion":""}`))
	})

	run, err := h.client(t).Run(context.Background(), testSpec(), 42)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.State() != StateRunning {
		t.Errorf("State() = %q, want running", run.State())
	}
}

func TestRunsDefaultsThePageSize(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
	})

	if _, err := h.client(t).Runs(context.Background(), testSpec(), 0); err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if h.lastQuery != "per_page=5" {
		t.Errorf("query = %q, want a defaulted per_page", h.lastQuery)
	}
}

func TestAServerErrorIsReportedAsARejection(t *testing.T) {
	// A 500 is GitHub's problem, not the reader's, but it still has to arrive
	// as a typed error with something readable in it.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream is unwell"))
	})

	_, err := h.client(t).User(context.Background())
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("err = %v, want the status text", err)
	}
}

func TestWithHTTPClientIsHonoured(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"someone"}`))
	})

	client, err := NewClient(testToken, WithBaseURL(h.URL), WithHTTPClient(&http.Client{Timeout: time.Minute}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.User(context.Background()); err != nil {
		t.Fatalf("User: %v", err)
	}
}

func TestWorkflowYAMLKeepsInputsOutOfTheShellScripts(t *testing.T) {
	// An input interpolated straight into a run block is a shell injection:
	// the value is whatever the person dispatching typed. The only place a
	// dispatch input may appear is an env: assignment, which arrives as a
	// quoted environment variable instead of as script text.
	for _, line := range strings.Split(WorkflowYAML, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "${{") {
			continue
		}

		allowed := strings.HasPrefix(trimmed, "TARGET:") ||
			strings.HasPrefix(trimmed, "IMAGES:") ||
			strings.HasPrefix(trimmed, "REGISTRY_USERNAME:") ||
			strings.HasPrefix(trimmed, "REGISTRY_PASSWORD:")

		if !allowed {
			t.Errorf("an expression appears outside env:, where the shell would see it: %q", trimmed)
		}
	}
}

func TestWorkflowYAMLDeclaresWhatItNeeds(t *testing.T) {
	for _, required := range []string{
		"workflow_dispatch:",
		"inputs:",
		"target:",
		"images:",
		"permissions:",
		"contents: read",
		"secrets.ACR_USERNAME",
		"secrets.ACR_PASSWORD",
		"docker pull",
		"docker push",
	} {
		if !strings.Contains(WorkflowYAML, required) {
			t.Errorf("the workflow is missing %q", required)
		}
	}

	// It copies images and nothing else, so it has no business checking the
	// repository out.
	if strings.Contains(WorkflowYAML, "actions/checkout") {
		t.Error("the workflow checks out the repository, which it never needs")
	}
}
