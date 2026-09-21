// Package ghsync drives the GitHub Actions workflow that relocates images.
//
// The panel is a remote control, not a registry. It cannot push an image
// anywhere: it has no route to the registries worth copying from, and giving it
// one would make it the thing this project's non-goals say it is not. What it
// can do is tell GitHub to run a job that does have a route, and then report
// what happened — which is what this package implements.
//
// Only the token is a secret here. The target registry and the images travel
// as workflow inputs so they show up in the run log, which is where someone
// debugging a failed copy will look first.
package ghsync

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Spec is which repository and workflow a run belongs to.
type Spec struct {
	// Owner is the account or organisation, e.g. "DC1024".
	Owner string

	// Repo is the repository name, e.g. "image-relay".
	Repo string

	// Workflow is the workflow file name, e.g. "relay.yml". The API accepts a
	// numeric id too; a file name is what a person can recognise.
	Workflow string

	// Ref is the branch or tag the workflow runs from. Empty means the
	// repository's default branch, which is what GitHub does with an omitted
	// ref.
	Ref string
}

// ErrInvalid wraps a rejected Spec, so a caller can tell bad input from a
// failed request.
var ErrInvalid = errors.New("invalid action spec")

var (
	ownerPattern    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,38})$`)
	repoPattern     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	workflowPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}$`)
	refPattern      = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)

// Validate reports whether the spec is usable.
//
// Checked before it reaches a URL, because these values are pasted in by hand
// and an owner containing a slash would silently address a different
// repository rather than fail.
func (s Spec) Validate() error {
	if !ownerPattern.MatchString(s.Owner) {
		return fmt.Errorf("%w: owner %q is not a GitHub account name", ErrInvalid, s.Owner)
	}
	if !repoPattern.MatchString(s.Repo) || strings.HasPrefix(s.Repo, ".") {
		return fmt.Errorf("%w: repository %q is not a repository name", ErrInvalid, s.Repo)
	}
	if !workflowPattern.MatchString(s.Workflow) {
		return fmt.Errorf("%w: workflow %q is not a workflow file name", ErrInvalid, s.Workflow)
	}
	if s.Ref != "" && !refPattern.MatchString(s.Ref) {
		return fmt.Errorf("%w: ref %q is not a branch or tag", ErrInvalid, s.Ref)
	}
	return nil
}

// Target is the registry and namespace copied images land in.
//
// Not a secret: it is a destination, and a run log that cannot say where it
// pushed to is a run log nobody can act on.
type Target struct {
	// Registry is the host, e.g. "registry.cn-hangzhou.aliyuncs.com".
	Registry string

	// Namespace is the repository namespace inside that registry.
	Namespace string
}

// Prefix renders the target as the path prefix the workflow prepends.
func (t Target) Prefix() string {
	return strings.Trim(t.Registry, "/ ") + "/" + strings.Trim(t.Namespace, "/ ")
}

var registryPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?$`)

// Validate reports whether the target is usable.
func (t Target) Validate() error {
	if !registryPattern.MatchString(strings.TrimSpace(t.Registry)) {
		return fmt.Errorf("%w: registry %q is not a host", ErrInvalid, t.Registry)
	}
	namespace := strings.TrimSpace(t.Namespace)
	if namespace == "" {
		return fmt.Errorf("%w: a namespace is required", ErrInvalid)
	}
	for _, part := range strings.Split(namespace, "/") {
		// "." and ".." would pass the character check and are not path
		// segments in any sense a registry would accept.
		if part == "." || part == ".." || !repoPattern.MatchString(part) {
			return fmt.Errorf("%w: namespace %q is not a valid path", ErrInvalid, t.Namespace)
		}
	}
	return nil
}

// Inputs are the workflow_dispatch values.
type Inputs struct {
	// Target is the destination prefix.
	Target string

	// Images are fully qualified image addresses, one per line. Newline
	// separated because that is what a person can paste, and because a
	// workflow_dispatch input is a string.
	Images []string
}

// Encode renders the inputs as the string map the API expects.
func (in Inputs) Encode() map[string]string {
	return map[string]string{
		"target": in.Target,
		"images": strings.Join(in.Images, "\n"),
	}
}

// Validate reports whether there is anything to run.
func (in Inputs) Validate() error {
	if strings.TrimSpace(in.Target) == "" {
		return fmt.Errorf("%w: no target registry was given", ErrInvalid)
	}
	if len(in.Images) == 0 {
		return fmt.Errorf("%w: no images were given", ErrInvalid)
	}
	for _, image := range in.Images {
		if strings.TrimSpace(image) == "" {
			return fmt.Errorf("%w: an image address is blank", ErrInvalid)
		}
	}
	return nil
}

// State is a run reduced to what a person needs to know.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StateUnknown   State = "unknown"
)

// Terminal reports whether a run in this state has stopped.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// User is the account a token belongs to.
type User struct {
	Login string
	Name  string
	URL   string
}

// Run is one workflow run.
type Run struct {
	ID         int64
	Status     string
	Conclusion string
	Event      string
	Title      string
	URL        string
	CreatedAt  time.Time
	UpdatedAt  time.Time

	// HeadBranch and HeadSHA say what was actually run. Two dispatches on the
	// same workflow can be queued at once, and this is the only thing in the
	// response that distinguishes them.
	HeadBranch string
	HeadSHA    string
}

// State reduces the API's two fields into one answer.
//
// GitHub reports status and conclusion separately, and conclusion is empty
// until the run finishes. Collapsing them here means every caller does not have
// to remember that a "completed" run with no conclusion is not a success.
func (r Run) State() State {
	switch r.Status {
	case "queued", "requested", "waiting", "pending":
		return StateQueued
	case "in_progress":
		return StateRunning
	case "completed":
		switch r.Conclusion {
		case "success":
			return StateSucceeded
		case "cancelled", "skipped":
			return StateCancelled
		case "failure", "timed_out", "startup_failure", "action_required", "":
			// An empty conclusion on a completed run is not a success we can
			// vouch for, so it is reported as a failure rather than glossed.
			return StateFailed
		}
	}
	return StateUnknown
}

// Job is one job inside a run.
type Job struct {
	ID          int64
	Name        string
	Status      string
	Conclusion  string
	URL         string
	StartedAt   time.Time
	CompletedAt time.Time
}

// State reduces a job's two fields the same way Run.State does.
func (j Job) State() State {
	return Run{Status: j.Status, Conclusion: j.Conclusion}.State()
}

// Duration is how long the job took, zero when it has not both started and
// finished. A job that is still running has no duration, which is not the same
// as a duration of zero.
func (j Job) Duration() time.Duration {
	if j.StartedAt.IsZero() || j.CompletedAt.IsZero() {
		return 0
	}
	return j.CompletedAt.Sub(j.StartedAt)
}
