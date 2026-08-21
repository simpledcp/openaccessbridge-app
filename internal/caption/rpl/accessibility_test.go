package rpl

import (
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// doc builds an RPL whose resources appear in the given (type, language) order,
// so a test can put the LESS accessible track first — the case that matters.
func doc(pairs ...[2]string) *RPL {
	var reel Reel
	for i, p := range pairs {
		reel.Resources = append(reel.Resources, Resource{
			ID: string(rune('a' + i)), RawType: p[0], Kind: kindFromType(p[0]), Language: p[1],
		})
	}
	return &RPL{Reels: []Reel{reel}}
}

func TestBestKindForPicksTheMostAccessibleTrack(t *testing.T) {
	r := doc([2]string{"MainSubtitle", "en"}, [2]string{"ClosedCaption", "en"})
	if got := r.BestKindFor("en"); got != model.TrackClosedCaption {
		t.Errorf("BestKindFor(en) = %q, want %q", got, model.TrackClosedCaption)
	}
	if got := r.BestKindFor("de"); got != model.TrackUnknown {
		t.Errorf("BestKindFor(de) = %q, want %q for a language the show does not carry",
			got, model.TrackUnknown)
	}
}

func TestLanguagesByAccessibility(t *testing.T) {
	cases := []struct {
		name string
		doc  *RPL
		want []string
	}{
		{
			name: "closed caption outranks an earlier subtitle",
			doc:  doc([2]string{"MainSubtitle", "fr"}, [2]string{"ClosedCaption", "en"}),
			want: []string{"en", "fr"},
		},
		{
			name: "equal kinds keep playlist order",
			doc:  doc([2]string{"ClosedCaption", "fr"}, [2]string{"ClosedCaption", "en"}),
			want: []string{"fr", "en"},
		},
		{
			name: "closed subtitle sits between closed caption and main subtitle",
			doc: doc([2]string{"MainSubtitle", "de"}, [2]string{"ClosedSubtitle", "fr"},
				[2]string{"ClosedCaption", "en"}),
			want: []string{"en", "fr", "de"},
		},
		{
			name: "a language's best track is what ranks it, not its first",
			doc: doc([2]string{"MainSubtitle", "fr"}, [2]string{"ClosedCaption", "fr"},
				[2]string{"ClosedSubtitle", "en"}),
			want: []string{"fr", "en"},
		},
		{
			// ...nor its LAST. Every other case here happens to list each
			// language's best track last, so "take the last one" would pass them
			// all — a mutation proved exactly that. Here French's best track is
			// listed FIRST and a worse one follows it, which is the ordinary shape
			// of a playlist that lists closed captions before translations.
			name: "a language's best track is what ranks it, not its last",
			doc: doc([2]string{"ClosedCaption", "fr"}, [2]string{"MainSubtitle", "fr"},
				[2]string{"ClosedSubtitle", "en"}),
			want: []string{"fr", "en"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.doc.LanguagesByAccessibility()
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
