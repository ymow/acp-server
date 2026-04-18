package gittwin

import (
	"fmt"
	"strings"
	"unicode"
)

// DiffStat is the minimal diff summary the mappers operate on.
// Extracted from a PR merge commit or a push diff.
type DiffStat struct {
	LinesAdded   int
	LinesRemoved int
	FilesChanged int
	// DiffBody is the full unified diff text. Only words_in_diff uses it.
	// Callers may leave it empty when they only need line-based mappers.
	DiffBody string
}

// UnitMapper is the ACR-400 Part 4 contract: given a diff, return a unit_count.
// Returning (0, nil) is legal and means "this change produces no billable units".
type UnitMapper func(DiffStat) (int, error)

// BuiltinMappers exposes the three v0.1 built-ins.
var BuiltinMappers = map[string]UnitMapper{
	"lines_added":                linesAdded,
	"lines_added_minus_removed":  linesAddedMinusRemoved,
	"words_in_diff":              wordsInDiff,
}

// Resolve returns the mapper for name, or an error if unknown.
// Empty name falls back to per-space_type defaults in DefaultMapperForSpace.
func Resolve(name string) (UnitMapper, error) {
	if name == "" {
		return nil, fmt.Errorf("unit_mapper: name required")
	}
	m, ok := BuiltinMappers[name]
	if !ok {
		return nil, fmt.Errorf("unit_mapper: unknown mapper %q (built-ins: lines_added, lines_added_minus_removed, words_in_diff)", name)
	}
	return m, nil
}

// DefaultMapperForSpace returns the ACR-400 Part 4 default for a SpaceType.
// Returns ("", false) when the space_type requires explicit configuration.
func DefaultMapperForSpace(spaceType string) (string, bool) {
	switch spaceType {
	case "book":
		return "words_in_diff", true
	case "code":
		return "lines_added_minus_removed", true
	case "research":
		// v0.1 approximation — "refs_added" would require parsing bib files,
		// deferred to Phase 4. Falls back to lines_added until then.
		return "lines_added", true
	case "music", "custom":
		return "", false
	}
	return "", false
}

func linesAdded(d DiffStat) (int, error) { return d.LinesAdded, nil }

func linesAddedMinusRemoved(d DiffStat) (int, error) {
	n := d.LinesAdded - d.LinesRemoved
	if n < 0 {
		// ACR-400 Part 4: removal never goes negative — deletions don't refund.
		n = 0
	}
	return n, nil
}

// wordsInDiff counts whitespace-separated tokens on lines added by the diff.
// Lines starting with "+" count; "+++" headers are skipped. It is intentionally
// dumb: no language awareness, no punctuation stripping. Covenants that need
// precise word counts should declare a custom mapper.
func wordsInDiff(d DiffStat) (int, error) {
	if d.DiffBody == "" {
		return 0, nil
	}
	total := 0
	for _, line := range strings.Split(d.DiffBody, "\n") {
		if len(line) == 0 || line[0] != '+' {
			continue
		}
		if strings.HasPrefix(line, "+++") {
			continue
		}
		// Strip the leading '+', count fields.
		body := line[1:]
		total += countFields(body)
	}
	return total, nil
}

// countFields is like strings.Fields but treats any non-letter/digit run as separator.
// Keeps punctuation-adjacent words in the count ("foo, bar" = 2).
func countFields(s string) int {
	n := 0
	inWord := false
	for _, r := range s {
		isWord := unicode.IsLetter(r) || unicode.IsDigit(r)
		if isWord && !inWord {
			n++
			inWord = true
		} else if !isWord {
			inWord = false
		}
	}
	return n
}
