package provider_test

import (
	"testing"
	"time"
)

// The aging suite's accounting helpers live in this untagged file, with their
// tests, so the tests run without a browser or network. e2e_aging_test.go
// shares them.

// potFetch is one /get_pot answer: when it arrived, the daemon's X-POT-Cache
// verdict, and the token it carried (empty for an error).
type potFetch struct {
	at      time.Time
	verdict string
	token   string
}

// agingTokenInstants dates token by the first fetch that returned it: fetched
// is when that answer arrived, and minted is hitMinted for a cache hit or the
// fetch itself for a miss, which minted it. Both are zero when no fetch
// returned token.
func agingTokenInstants(fetches []potFetch, token string, hitMinted time.Time) (fetched, minted time.Time) {
	for _, f := range fetches {
		if token == "" || f.token != token {
			continue
		}
		if f.verdict == "hit" {
			return f.at, hitMinted
		}
		return f.at, f.at
	}
	return time.Time{}, time.Time{}
}

// agingAge is how old an artifact was when the stream began: "-" when either
// instant is missing, and "in-stream" for one made after the stream began.
func agingAge(from, to time.Time) string {
	switch {
	case from.IsZero() || to.IsZero():
		return "-"
	case from.After(to):
		return "in-stream"
	}
	return to.Sub(from).Round(time.Millisecond).String()
}

// TestAgingTokenInstants pins how the aging suite dates the streamed token: by
// the first fetch that returned it, with a cache hit minted before that fetch.
func TestAgingTokenInstants(t *testing.T) {
	warm := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return warm.Add(time.Duration(s) * time.Second) }
	tests := []struct {
		name            string
		fetches         []potFetch
		token           string
		hitMinted       time.Time
		fetched, minted time.Time
	}{
		{"a hit returns an earlier mint", []potFetch{{at(5), "hit", "A"}}, "A", warm, at(5), warm},
		{"a miss mints during the fetch", []potFetch{{at(5), "miss", "A"}}, "A", warm, at(5), at(5)},
		{"the first fetch of the token dates it", []potFetch{{at(5), "miss", "A"}, {at(9), "hit", "A"}}, "A", warm, at(5), at(5)},
		{"a later token is dated by its own fetch", []potFetch{{at(5), "hit", "A"}, {at(9), "miss", "B"}}, "B", warm, at(9), at(9)},
		{"an error answer is skipped", []potFetch{{at(5), "unknown", ""}, {at(9), "hit", "A"}}, "A", warm, at(9), warm},
		{"a hit with no known mint stays undated", []potFetch{{at(5), "hit", "A"}}, "A", time.Time{}, at(5), time.Time{}},
		{"no token was streamed", []potFetch{{at(5), "unknown", ""}}, "", warm, time.Time{}, time.Time{}},
		{"no fetch returned the token", []potFetch{{at(5), "hit", "A"}}, "B", warm, time.Time{}, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetched, minted := agingTokenInstants(tt.fetches, tt.token, tt.hitMinted)
			if !fetched.Equal(tt.fetched) || !minted.Equal(tt.minted) {
				t.Errorf("fetched, minted = %v, %v; want %v, %v", fetched, minted, tt.fetched, tt.minted)
			}
		})
	}
}

// TestAgingAge pins the summary's ages: an artifact made after the stream began
// reads "in-stream", never a negative duration.
func TestAgingAge(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		from, to time.Time
		want     string
	}{
		{start.Add(-1500 * time.Millisecond), start, "1.5s"},
		{start, start, "0s"},
		{start.Add(2 * time.Second), start, "in-stream"},
		{time.Time{}, start, "-"},
		{start, time.Time{}, "-"},
	}
	for _, tt := range tests {
		if got := agingAge(tt.from, tt.to); got != tt.want {
			t.Errorf("agingAge(%v, %v) = %q, want %q", tt.from, tt.to, got, tt.want)
		}
	}
}
