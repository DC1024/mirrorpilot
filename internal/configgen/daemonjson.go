package configgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// member is one key of a JSON object, kept in the order it was written.
//
// A map would be simpler and would sort its keys, which means handing someone
// back a configuration file whose settings have silently moved. For a file a
// person is about to paste into /etc/docker, that is the kind of change that
// costs ten minutes of staring at a diff.
type member struct {
	key   string
	value json.RawMessage
}

// object is a JSON object that remembers its key order.
type object []member

// marshalJSON renders the object compactly. Indentation is left to
// json.MarshalIndent, which re-indents whatever this returns — including the
// values carried through as RawMessage, so a nested object survives with its
// shape intact.
func (o object) marshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')

	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}

		key, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')

		// A value that arrived as raw bytes is compacted rather than trusted:
		// whitespace from the original file would otherwise leak into the
		// output and defeat the re-indentation.
		var compact bytes.Buffer
		if err := json.Compact(&compact, m.value); err != nil {
			return nil, fmt.Errorf("value for %q is not valid JSON: %w", m.key, err)
		}
		buf.Write(compact.Bytes())
	}

	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// MarshalJSON makes object usable directly with encoding/json.
func (o object) MarshalJSON() ([]byte, error) { return o.marshalJSON() }

// get returns a member's value.
func (o object) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}

// set returns a copy of the object with key assigned.
//
// An existing key keeps its position, so the one line someone came to change
// is the one line that moves. A new key is appended.
func (o object) set(key string, value json.RawMessage) object {
	out := make(object, 0, len(o)+1)

	replaced := false
	for _, m := range o {
		if m.key == key {
			out = append(out, member{key: key, value: value})
			replaced = true
			continue
		}
		out = append(out, m)
	}
	if !replaced {
		out = append(out, member{key: key, value: value})
	}
	return out
}

// parseObject reads a JSON object, preserving key order.
//
// Validation and ordering are two separate steps on purpose. json.Valid gives
// one clear answer for the whole document — including the two mistakes people
// actually make in daemon.json, comments and trailing commas, both of which
// Docker rejects — and the token walk afterwards can then assume it is already
// looking at well-formed JSON.
func parseObject(src string) (object, error) {
	trimmed := strings.TrimSpace(src)
	if trimmed == "" {
		return object{}, nil
	}

	if !json.Valid([]byte(trimmed)) {
		return nil, errors.New("not a JSON document (comments and trailing commas are not allowed in daemon.json)")
	}

	dec := json.NewDecoder(strings.NewReader(trimmed))

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("the top level must be a JSON object")
	}

	var out object
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("an object key is not a string")
		}

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, member{key: key, value: raw})
	}

	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// stringList reads a value that ought to be an array of strings.
//
// It reports an error rather than guessing when the shape is wrong. The
// alternative — replacing a setting we did not understand with one we do — is
// how a tool quietly deletes a configuration somebody had already made.
func stringList(raw json.RawMessage, key string) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%q is not an array of strings: %w", key, err)
	}
	return values, nil
}

// DaemonJSON renders a daemon.json for Docker Engine and Docker Desktop.
//
// existing, when not empty, is the reader's current daemon.json. Everything in
// it that this function does not manage is carried through unchanged, which is
// what makes the output something a person can paste over their own file
// instead of a document they have to diff by hand first.
//
// registry-mirrors is always replaced: it is the setting the caller came here
// to produce. insecure-registries is merged rather than replaced, because a
// plain-HTTP mirror belonging to the reader is not ours to remove.
func DaemonJSON(mirrors []Mirror, existing string) (string, []Warning, error) {
	ranked, warnings := Select(mirrors)
	if len(ranked) == 0 {
		return "", warnings, ErrNoMirrors
	}

	base, err := parseObject(existing)
	if err != nil {
		return "", warnings, fmt.Errorf("%w: %v", ErrExisting, err)
	}

	managed := insecureHosts(ranked)

	// Computed before the first set, because set returns a copy and reading
	// the caller's values afterwards would read the file we are replacing.
	var carried []string
	if len(managed) > 0 {
		if raw, ok := base.get("insecure-registries"); ok {
			carried, err = stringList(raw, "insecure-registries")
			if err != nil {
				return "", warnings, fmt.Errorf("%w: %v", ErrExisting, err)
			}
		}
	}

	out := base.set("registry-mirrors", mustMarshal(endpoints(ranked)))

	if len(managed) > 0 {
		out = out.set("insecure-registries", mustMarshal(mergeUnique(carried, managed)))
	}

	text, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", warnings, fmt.Errorf("configgen: render daemon.json: %w", err)
	}

	return string(text) + "\n", warnings, nil
}

// mergeUnique appends the values of extra that first is not already carrying.
//
// The comparison is case-insensitive: registry hostnames are, and someone
// retyping "Docker.IO" should not produce two entries for one host.
func mergeUnique(first, extra []string) []string {
	out := make([]string, 0, len(first)+len(extra))
	seen := make(map[string]bool, len(first)+len(extra))

	for _, value := range append(append([]string{}, first...), extra...) {
		key := strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

// mustMarshal encodes a value this package built itself. The only way it can
// fail is a type error in this file, which is a compile-shaped mistake that
// happens to be expressible through a slice, so a panic here is a self-test
// rather than a runtime condition.
func mustMarshal(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("configgen: cannot encode %T: %v", value, err))
	}
	return raw
}
