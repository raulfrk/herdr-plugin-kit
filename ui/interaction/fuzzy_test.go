package interaction

import (
	"slices"
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

func TestMatchScoringRewardsSemanticBoundariesAndAdjacency(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		positions []int
		score     int
	}{
		{name: "exact adjacent", text: "ab", positions: []int{0, 1}, score: 1132},
		{name: "word boundary adjacent", text: "x-abz", positions: []int{2, 3}, score: 1021},
		{name: "separate boundaries", text: "a_b", positions: []int{0, 2}, score: 1023},
		{name: "internal letters", text: "zab", positions: []int{1, 2}, score: 1015},
	}
	for _, test := range tests {
		match, ok := Match("ab", test.text)
		if !ok || !slices.Equal(match.Positions, test.positions) || match.Score != test.score {
			t.Fatalf("%s: Match(ab,%q)=(%+v,%t), want positions=%v score=%d", test.name, test.text, match, ok, test.positions, test.score)
		}
	}

	ranked := Rank("ab", []Candidate{{Key: "internal", Text: "zab"}, {Key: "adjacent", Text: "x-abz"}, {Key: "boundaries", Text: "a_b"}, {Key: "exact", Text: "ab"}})
	wantKeys := []string{"exact", "boundaries", "adjacent", "internal"}
	gotKeys := make([]string, len(ranked))
	for index, match := range ranked {
		gotKeys[index] = match.Candidate.Key
	}
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("semantic rank order = %v, want %v", gotKeys, wantKeys)
	}
}

func TestMatchRejectsMissingOrOutOfOrderSubsequence(t *testing.T) {
	for _, text := range []string{"", "a", "ba", "ac"} {
		if match, ok := Match("ab", text); ok {
			t.Fatalf("Match(ab,%q) unexpectedly matched at %v", text, match.Positions)
		}
	}
	match, ok := Match("", "anything")
	if !ok || len(match.Positions) != 0 || match.Score != 0 {
		t.Fatalf("empty query result = (%+v,%t)", match, ok)
	}
}
