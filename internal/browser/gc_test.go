package browser

import (
	"context"
	"testing"
	"time"
)

// gcTestDelay is the idle delay these tests run with: long enough that work
// done back to back lands inside one window, short enough to wait out.
const gcTestDelay = 40 * time.Millisecond

// waitCollections polls until the page has been collected want times.
func waitCollections(t *testing.T, page fakePage, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for page.collectionCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("collections = %d, want %d", page.collectionCount(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// Contexts served back to back share one collection, taken after the last one
// returns, so none of them pays for it.
func TestPlayerContextCollectsGarbageOnceThePageIsQuiet(t *testing.T) {
	page := newFakePageFor("vid", map[string][]fakeStep{
		playerContextExtractJS: {jsStringified(t, establishedPayload("vid", 635))},
		playerBufferedJS:       {jsStringified(t, bufferedPayload(103, 140, true))},
	})
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay
	s.establishedStreaming = true

	for range 3 {
		if _, err := s.PlayerContext(context.Background(), "vid"); err != nil {
			t.Fatalf("PlayerContext: %v", err)
		}
	}
	if got := page.collectionCount(); got != 0 {
		t.Fatalf("collections = %d right after the last context, want 0: the collection must not run inline", got)
	}
	waitCollections(t, page, 1)
	time.Sleep(3 * gcTestDelay)
	if got := page.collectionCount(); got != 1 {
		t.Errorf("collections = %d, want 1 for the whole burst", got)
	}
}

// Work that starts inside the window holds the collection off until it ends,
// so a proof or a context never shares the page with one.
func TestCollectionWaitsForPageWorkToEnd(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay

	s.pageWork(true)() // earlier work arms the collection
	done := s.pageWork(true)
	time.Sleep(3 * gcTestDelay)
	if got := page.collectionCount(); got != 0 {
		t.Fatalf("collections = %d while work held the page, want 0", got)
	}
	done()
	waitCollections(t, page, 1)
}

// Nested work is one stretch: a context that proves first is not collected
// between its proof and its own playback.
func TestCollectionWaitsForTheOutermostWork(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay

	outer := s.pageWork(true)
	s.pageWork(true)()
	time.Sleep(3 * gcTestDelay)
	if got := page.collectionCount(); got != 0 {
		t.Fatalf("collections = %d after the inner work ended, want 0 until the outer one does", got)
	}
	outer()
	waitCollections(t, page, 1)
}

// A closed session's page is gone, so its pending collection is dropped and
// late work cannot arm another.
func TestCloseDropsThePendingCollection(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay

	s.pageWork(true)()
	late := s.pageWork(true) // a request that outlives the session
	s.Close()
	late()
	time.Sleep(3 * gcTestDelay)
	if got := page.collectionCount(); got != 0 {
		t.Errorf("collections = %d after Close, want 0", got)
	}
}

// A timer can fire as work begins, since stopping it does not recall a
// callback already on its way. The collection checks again when it runs, so it
// never shares the page with a proof or a context.
func TestCollectionThatFiresDuringWorkIsDropped(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay

	done := s.pageWork(true)
	s.collectGarbage() // the timer's callback, landing mid-work
	if got := page.collectionCount(); got != 0 {
		t.Fatalf("collections = %d during work, want 0", got)
	}
	done()
	waitCollections(t, page, 1)
}

// A callback left over from an earlier quiet stretch does not collect before
// the current one is due.
func TestCollectionWaitsForTheCurrentQuietStretch(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay

	s.pageWork(true)()
	s.collectGarbage() // a stale firing, the moment work ended
	if got := page.collectionCount(); got != 0 {
		t.Fatalf("collections = %d the moment work ended, want 0 until the delay has passed", got)
	}
	waitCollections(t, page, 1)
}

// A callback that outlives the session does not reach its page.
func TestCollectionAfterCloseIsDropped(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay

	s.pageWork(true)()
	s.Close()
	time.Sleep(2 * gcTestDelay) // past due, so only Close holds it back
	s.collectGarbage()
	if got := page.collectionCount(); got != 0 {
		t.Errorf("collections = %d after Close, want 0", got)
	}
}

// A mint leaves next to nothing behind, so a daemon that only mints is never
// collected: the pause would buy no memory.
func TestMintsAloneAreNeverCollected(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay

	for range 5 {
		s.pageWork(false)()
	}
	time.Sleep(3 * gcTestDelay)
	if got := page.collectionCount(); got != 0 {
		t.Errorf("collections = %d after mints alone, want 0", got)
	}
}

// A mint still counts as page work: it holds a pending collection off, and the
// quiet stretch starts over when it ends.
func TestMintHoldsOffAPendingCollection(t *testing.T) {
	page := newFakePage(nil)
	s := newFakeSession(page)
	s.timing.gcDelay = gcTestDelay

	s.pageWork(true)() // playback leaves a collection pending
	mint := s.pageWork(false)
	time.Sleep(3 * gcTestDelay)
	if got := page.collectionCount(); got != 0 {
		t.Fatalf("collections = %d while a mint held the page, want 0", got)
	}
	mint()
	waitCollections(t, page, 1)
}
