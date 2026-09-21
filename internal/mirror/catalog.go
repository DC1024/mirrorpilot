package mirror

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// builtinJSON is the catalogue that ships with every build.
//
// It is embedded rather than fetched, and rather than read from disk at
// runtime: a source list that can be swapped out after the fact is
// configuration an attacker would love to control, because every entry in it is
// an endpoint users are told to route their image pulls through.
//
// Names and notes in here are English free text rather than i18n keys. They are
// operator metadata — proper nouns and factual descriptions of third parties —
// not panel copy, and routing them through the translation catalogue would make
// the dead-key check fight data that is supposed to change as mirrors come and
// go.
//
//go:embed sources.json
var builtinJSON []byte

// Builtin parses and validates the embedded catalogue.
//
// A failure here is a programming error, not a runtime condition: it means the
// file committed to the repository does not satisfy Validate. That is why it is
// surfaced as an error rather than silently yielding an empty catalogue — an
// empty mirror list would look like a working panel with nothing to show.
func Builtin() (*Catalog, error) {
	var sources []Source
	if err := json.Unmarshal(builtinJSON, &sources); err != nil {
		return nil, fmt.Errorf("mirror: parse built-in catalogue: %w", err)
	}

	catalog, err := New(sources)
	if err != nil {
		return nil, fmt.Errorf("mirror: built-in catalogue is invalid: %w", err)
	}
	return catalog, nil
}
