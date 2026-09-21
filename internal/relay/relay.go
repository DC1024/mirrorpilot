// Package relay spells out image addresses for a relay.
//
// A relay is a different kind of thing from a registry mirror, and the
// difference is entirely a matter of which address shape it answers:
//
//   - A mirror is configured in daemon.json and receives the registry protocol
//     at the same paths the upstream uses. You hand Docker the mirror and keep
//     asking for "alpine".
//   - A relay takes the whole original address as a path. You ask it for
//     "docker.io/library/alpine" and it decides where to fetch from.
//
// The second shape is the one Huawei Cloud's ddn-k8s transit service uses and
// the one DaoCloud's crproxy implements, which is why a single generator
// covers both. Conflating the two is the most common way to end up with a
// configuration that looks right and pulls nothing, so this package names the
// distinction out loud.
//
// Nothing here runs a relay. This project is not a registry proxy and does not
// fetch images; it writes down the address a relay would answer on.
package relay

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ErrInvalid wraps every rejection so a caller can tell bad input from a bug.
var ErrInvalid = errors.New("invalid image reference")

// Known upstreams. The list exists to offer a default and to make the origin
// field of the UI a picker rather than free text; a relay is not limited to
// these, and Parse accepts any first component that looks like a registry.
var origins = []string{
	"docker.io",
	"ghcr.io",
	"quay.io",
	"gcr.io",
	"registry.k8s.io",
	"mcr.microsoft.com",
}

// Origins lists the upstreams the panel offers as a starting point.
func Origins() []string {
	out := make([]string, len(origins))
	copy(out, origins)
	return out
}

// dockerHub is the origin that gets special treatment: it is the default, and
// its official images live under an implicit "library/" namespace.
const dockerHub = "docker.io"

// Reference is a fully qualified image address, split into the three parts a
// relay needs.
type Reference struct {
	// Origin is the registry host, e.g. "docker.io" or "ghcr.io".
	Origin string

	// Repository is the path below the registry, e.g. "library/alpine". The
	// implicit library/ namespace of Docker Hub is made explicit here, because
	// a relay needs the address it would have fetched, not the shorthand a
	// person types.
	Repository string

	// Tag is the tag, empty when the input carried none. Mutually exclusive
	// with Digest.
	Tag string

	// Digest is the digest, empty when the input carried none. A pinned digest
	// beats a tag: it is the only form that names the bytes.
	Digest string
}

// String renders the fully qualified address, which is what a relay expects to
// find in its path.
func (r Reference) String() string {
	address := r.Origin + "/" + r.Repository
	switch {
	case r.Digest != "":
		return address + "@" + r.Digest
	case r.Tag != "":
		return address + ":" + r.Tag
	default:
		return address
	}
}

// Default returns the address used when the panel has nothing better to offer.
func Default() Reference {
	return Reference{Origin: dockerHub, Repository: "library/alpine", Tag: "latest"}
}

var (
	// A path segment. Kept deliberately a little looser than the distribution
	// specification: the point is to reject what Docker itself rejects —
	// uppercase, spaces, empty segments — not to re-litigate the grammar.
	segmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

	tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

	digestPattern = regexp.MustCompile(`^[a-z0-9]+:[0-9a-fA-F]{32,}$`)

	// hostPattern is what makes a first component a registry rather than a
	// namespace. It is Docker's own rule: a dot, a port, or the literal
	// "localhost".
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?(:[0-9]+)?$`)
)

// Parse reads an image address in any of the forms a person might paste.
//
// It is deliberately strict about the things Docker is strict about. Accepting
// "Docker.IO/Library/Alpine" and quietly lowercasing it would produce a
// document that disagrees with the one the reader typed, and the reader would
// find out when the pull failed.
//
// The order of the three splits is what makes the awkward cases work: the
// digest comes off first because "@" is unambiguous, then the first path
// segment is classified as a registry or a namespace, and only then is a tag
// taken from the last segment. Doing the tag first would read "localhost:5000"
// as an image called "localhost" tagged "5000".
func Parse(input string) (Reference, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Reference{}, fmt.Errorf("%w: nothing was given", ErrInvalid)
	}
	if strings.ContainsAny(raw, " \t\n") {
		return Reference{}, fmt.Errorf("%w: %q contains whitespace", ErrInvalid, raw)
	}
	if strings.Contains(raw, "://") {
		return Reference{}, fmt.Errorf(
			"%w: %q is a URL; an image address has no scheme", ErrInvalid, raw)
	}
	if strings.HasPrefix(raw, "/") || strings.HasSuffix(raw, "/") {
		return Reference{}, fmt.Errorf("%w: %q has an empty path segment", ErrInvalid, raw)
	}

	ref := Reference{}

	if before, digest, found := strings.Cut(raw, "@"); found {
		if !digestPattern.MatchString(digest) {
			return Reference{}, fmt.Errorf("%w: %q is not a digest", ErrInvalid, digest)
		}
		raw, ref.Digest = before, digest
	}

	parts := strings.Split(raw, "/")

	// Only a first segment followed by something can be a registry. A lone
	// component is a repository name, whatever colons it carries — which is
	// what Docker does, and why "myregistry:5000" is an image tagged 5000.
	if len(parts) >= 2 && looksLikeRegistry(parts[0]) {
		ref.Origin = parts[0]
		parts = parts[1:]
	}

	last := len(parts) - 1
	if repository, tag, found := strings.Cut(parts[last], ":"); found {
		if !tagPattern.MatchString(tag) {
			return Reference{}, fmt.Errorf("%w: %q is not a tag", ErrInvalid, tag)
		}
		ref.Tag = tag
		parts[last] = repository
	}

	if ref.Origin == "" {
		ref.Origin = dockerHub
	}

	// Docker Hub's official images live under an implicit library/ namespace.
	// A relay needs the address it would actually fetch, so it is spelled out.
	if ref.Origin == dockerHub && len(parts) == 1 {
		parts = append([]string{"library"}, parts...)
	}

	for _, part := range parts {
		if !segmentPattern.MatchString(part) {
			return Reference{}, fmt.Errorf(
				"%w: %q is not a valid repository segment (lowercase letters, digits, '.', '_' and '-')",
				ErrInvalid, part)
		}
	}
	ref.Repository = strings.Join(parts, "/")

	return ref, nil
}

// looksLikeRegistry applies Docker's rule for telling a registry from a
// namespace: a dot in the host, a port, or the literal "localhost".
//
// hostPattern has already guaranteed that anything after a colon is digits, so
// a colon that survived it is a port and not the tag separator.
func looksLikeRegistry(component string) bool {
	if component == "localhost" {
		return true
	}
	if !hostPattern.MatchString(component) {
		return false
	}

	host, _, hasPort := strings.Cut(component, ":")
	return hasPort || strings.Contains(host, ".")
}

// Endpoint normalises a relay address to a lowercase host, optionally followed
// by a path prefix.
//
// The prefix is not optional decoration: Huawei Cloud's ddn-k8s transit
// service answers on "<host>/ddn-k8s/<image>", so a relay endpoint is a base
// path and not simply a host. It also may not be an image path — that is the
// thing being appended — which is why the split has to be spelled out rather
// than guessed at.
func Endpoint(input string) (string, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return "", fmt.Errorf("%w: no relay address was given", ErrInvalid)
	}
	if strings.ContainsAny(raw, " \t\n") {
		return "", fmt.Errorf("%w: %q contains whitespace", ErrInvalid, input)
	}

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("%w: %q is not a usable address", ErrInvalid, input)
		}

		raw = u.Host
		if prefix := strings.Trim(u.Path, "/"); prefix != "" {
			raw += "/" + prefix
		}
	}

	raw = strings.ToLower(strings.Trim(raw, "/"))

	parts := strings.Split(raw, "/")
	if !hostPattern.MatchString(parts[0]) {
		return "", fmt.Errorf("%w: %q does not begin with a host", ErrInvalid, input)
	}
	for _, part := range parts[1:] {
		if !segmentPattern.MatchString(part) {
			return "", fmt.Errorf("%w: %q has an unusable path segment %q", ErrInvalid, input, part)
		}
	}

	return strings.Join(parts, "/"), nil
}

// Relayed returns the address to hand Docker when pulling through a relay.
//
// The shape is "<relay>/<original address>": the upstream's own host stays in
// the path, which is what lets one relay serve every upstream it knows about.
func Relayed(endpoint string, ref Reference) (string, error) {
	base, err := Endpoint(endpoint)
	if err != nil {
		return "", err
	}
	if ref.Origin == "" || ref.Repository == "" {
		return "", fmt.Errorf("%w: the image reference is incomplete", ErrInvalid)
	}
	return base + "/" + ref.String(), nil
}

// PullCommand renders the copy-pasteable pull for a relayed address.
func PullCommand(endpoint string, ref Reference) (string, error) {
	relayed, err := Relayed(endpoint, ref)
	if err != nil {
		return "", err
	}
	return "docker pull " + relayed, nil
}
