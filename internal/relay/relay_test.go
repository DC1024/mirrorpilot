package relay

import (
	"errors"
	"strings"
	"testing"
)

func TestParseFillsInTheDockerHubDefaults(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"alpine", "docker.io/library/alpine"},
		{"alpine:3.21", "docker.io/library/alpine:3.21"},
		{"library/alpine", "docker.io/library/alpine"},
		{"myuser/myrepo", "docker.io/myuser/myrepo"},
		{"myuser/myrepo:v1", "docker.io/myuser/myrepo:v1"},
		{"  alpine:3.21  ", "docker.io/library/alpine:3.21"},
	} {
		ref, err := Parse(tc.input)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.input, err)
			continue
		}
		if got := ref.String(); got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseKeepsAnExplicitOrigin(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"ghcr.io/owner/repo", "ghcr.io/owner/repo"},
		{"ghcr.io/owner/repo:1.2.3", "ghcr.io/owner/repo:1.2.3"},
		{"registry.k8s.io/pause:3.9", "registry.k8s.io/pause:3.9"},
		{"quay.io/org/img", "quay.io/org/img"},
		{"mcr.microsoft.com/dotnet/runtime:8.0", "mcr.microsoft.com/dotnet/runtime:8.0"},
		// A port in the host belongs to the host, not to the tag.
		{"localhost:5000/foo", "localhost:5000/foo"},
		{"myregistry:5000/foo:bar", "myregistry:5000/foo:bar"},
	} {
		ref, err := Parse(tc.input)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.input, err)
			continue
		}
		if got := ref.String(); got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseTreatsALoneComponentAsARepositoryNotARegistry(t *testing.T) {
	// Docker's rule: a registry has to be followed by something. Without that,
	// "myregistry:5000" would look like a host and the tag would vanish.
	ref, err := Parse("myregistry:5000")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ref.Origin != "docker.io" || ref.Tag != "5000" {
		t.Errorf("Parse = %+v, want origin docker.io and tag 5000", ref)
	}
}

func TestParseKeepsADigestAndIgnoresNoTag(t *testing.T) {
	const digest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	ref, err := Parse("alpine@" + digest)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ref.Digest != digest || ref.Tag != "" {
		t.Errorf("Parse = %+v, want the digest and no tag", ref)
	}
	if got, want := ref.String(), "docker.io/library/alpine@"+digest; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestParseRejectsWhatDockerWouldReject(t *testing.T) {
	// Each of these would either fail at pull time or name something other
	// than what was typed. Accepting them and repairing quietly is how a
	// generated address ends up disagreeing with the reference beside it.
	for name, input := range map[string]string{
		"empty":                   "",
		"whitespace only":         "   ",
		"inner whitespace":        "alpine 3.21",
		"a URL":                   "https://ghcr.io/owner/repo",
		"uppercase repository":    "Alpine",
		"uppercase namespace":     "ghcr.io/OWNER/repo",
		"empty tag":               "alpine:",
		"leading slash":           "/alpine",
		"trailing slash":          "alpine/",
		"double slash":            "ghcr.io//repo",
		"digest too short":        "alpine@sha256:abc123",
		"blank path segment":      "ghcr.io/owner//repo",
		"segment starting with a": "ghcr.io/owner/-repo",
	} {
		if _, err := Parse(input); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Parse(%q) err = %v, want ErrInvalid", name, input, err)
		}
	}
}

func TestParseReadsALoneComponentWithADotAsARepository(t *testing.T) {
	// Docker only treats the first component as a registry when something
	// follows it, so "ghcr.io" on its own is an image name and not a host that
	// forgot its path. Documented because it looks wrong at a glance.
	ref, err := Parse("ghcr.io")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := ref.String(), "docker.io/library/ghcr.io"; got != want {
		t.Errorf("Parse = %q, want %q", got, want)
	}
}

func TestEndpointAcceptsTheFormsPeopleActuallyHave(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"docker.example", "docker.example"},
		{"docker.example:8443", "docker.example:8443"},
		{"https://docker.example", "docker.example"},
		{"https://docker.example/", "docker.example"},
		{"HTTP://Docker.Example:8443", "docker.example:8443"},
		// The ddn-k8s transit service answers under a path prefix, so an
		// endpoint is a base path and not just a host.
		{"swr.cn-north-4.myhuaweicloud.com/ddn-k8s", "swr.cn-north-4.myhuaweicloud.com/ddn-k8s"},
		{"https://swr.cn-north-4.myhuaweicloud.com/ddn-k8s/", "swr.cn-north-4.myhuaweicloud.com/ddn-k8s"},
		{"relay:5000", "relay:5000"},
	} {
		got, err := Endpoint(tc.input)
		if err != nil {
			t.Errorf("Endpoint(%q): %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Endpoint(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestEndpointRejectsWhatIsNotAnAddress(t *testing.T) {
	for name, input := range map[string]string{
		"empty":            "",
		"whitespace":       "  ",
		"spaces":           "docker example",
		"uppercase only":   "Not A Host",
		"scheme with host": "https://",
		"bad prefix":       "docker.example/DDN_K8S path",
	} {
		if _, err := Endpoint(input); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Endpoint(%q) err = %v, want ErrInvalid", name, input, err)
		}
	}
}

func TestRelayedPutsTheWholeOriginalAddressInThePath(t *testing.T) {
	ref, err := Parse("alpine:3.21")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, err := Relayed("swr.cn-north-4.myhuaweicloud.com/ddn-k8s", ref)
	if err != nil {
		t.Fatalf("Relayed: %v", err)
	}

	want := "swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/alpine:3.21"
	if got != want {
		t.Errorf("Relayed = %q, want %q", got, want)
	}
}

func TestPullCommandIsCopyPasteable(t *testing.T) {
	ref, err := Parse("ghcr.io/owner/repo:1.2.3")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got, err := PullCommand("relay.example", ref)
	if err != nil {
		t.Fatalf("PullCommand: %v", err)
	}

	want := "docker pull relay.example/ghcr.io/owner/repo:1.2.3"
	if got != want {
		t.Errorf("PullCommand = %q, want %q", got, want)
	}
}

func TestRelayedRefusesAnIncompleteReference(t *testing.T) {
	if _, err := Relayed("relay.example", Reference{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
	if _, err := Relayed("", Reference{Origin: "docker.io", Repository: "library/alpine"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid for an empty endpoint", err)
	}
}

func TestDefaultIsUsable(t *testing.T) {
	ref := Default()

	if _, err := Relayed("relay.example", ref); err != nil {
		t.Fatalf("the default reference cannot be relayed: %v", err)
	}
	if !strings.HasPrefix(ref.String(), "docker.io/") {
		t.Errorf("default = %q, want a Docker Hub address", ref.String())
	}
}

func TestOriginsIsACopy(t *testing.T) {
	// A caller that sorts or trims the list in place must not be able to
	// change what the next caller sees.
	first := Origins()
	first[0] = "mutated"

	if Origins()[0] == "mutated" {
		t.Error("Origins handed out its own slice")
	}
}
