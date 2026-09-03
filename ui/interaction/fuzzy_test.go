package interaction

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestRankIsUnicodeAwareDeterministicAndStable(t *testing.T) {
	candidates := []Candidate{{Key: "late", Text: "z alpha beta"}, {Key: "boundary", Text: "alpha beta"}, {Key: "tie-1", Text: "ábaco"}, {Key: "tie-2", Text: "ábaco"}}
	first := Rank("ab", candidates)
	second := Rank("ab", candidates)
	if len(first) != 2 || first[0].Candidate.Key != "boundary" || first[1].Candidate.Key != "late" {
		t.Fatalf("ranked matches = %#v", first)
	}
	if len(second) != len(first) || second[0].Score != first[0].Score {
		t.Fatalf("nondeterministic ranks = %#v / %#v", first, second)
	}
	unicodeMatches := Rank("ÁB", candidates)
	if len(unicodeMatches) != 2 || unicodeMatches[0].Candidate.Key != "tie-1" || unicodeMatches[1].Candidate.Key != "tie-2" {
		t.Fatalf("Unicode stable ties = %#v", unicodeMatches)
	}
}

func TestPropertyMatchPositionsReconstructFoldedQuery(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		text := rapid.StringMatching(`[a-zA-Z0-9 _-]{0,80}`).Draw(t, "text")
		query := rapid.StringMatching(`[a-zA-Z0-9]{0,12}`).Draw(t, "query")
		match, ok := Match(query, text)
		if !ok {
			return
		}
		runes := []rune(strings.ToLower(text))
		var rebuilt []rune
		previous := -1
		for _, position := range match.Positions {
			if position <= previous || position < 0 || position >= len(runes) {
				t.Fatalf("invalid positions %v for %q", match.Positions, text)
			}
			rebuilt = append(rebuilt, runes[position])
			previous = position
		}
		if string(rebuilt) != strings.ToLower(query) {
			t.Fatalf("positions reconstructed %q, want %q", rebuilt, strings.ToLower(query))
		}
	})
}
