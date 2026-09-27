// Package minter adds caching, retries, crash recovery, and tenant routing to
// browser sessions.
package minter

import (
	"context"
	"errors"
	"fmt"
	"github.com/colespringer/waxseal/internal/browser"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Minter adds token caching, single-flight attestation, retries, crash recovery,
// session recycling, and metrics to one browser identity. Mint and PlayerContext
// calls serialize because they share one page. Tenants manages multiple Minters.
type Minter struct {
	video           string
	opts            browser.Options
	log             *slog.Logger
	maxAge          time.Duration // recycle the session once it is older than this
	streamingMaxAge time.Duration // recycle on the next streaming handoff once older than this; 0 disables
	reportDebounce  time.Duration // refill interval of the report budget (see ReportBurst)
	mintSeparation  time.Duration // spacing kept between an in-page mint and a context establishment

	// skipPremint turns off the mint ensure performs at attestation. Only
	// InjectSessionForTest sets it, so a dependent package's injected session sees
	// only the mints its own request drives.
	skipPremint bool

	// launch starts and attests a session. Tests replace it so the reliability
	// logic can run without a browser.
	launch func(ctx context.Context) (minterSession, error)

	mu   sync.Mutex
	sess minterSession
	// closed marks the Minter terminal: ensure refuses to launch once Close sets
	// it (see Close).
	closed     bool
	gen        uint64 // bumps on each (re)attest; invalidates older cache entries
	attestedAt time.Time
	// grantExpiresAt is when the current generation's tokens expire. Every token
	// an attestation mints carries the same expiry, so one mint reveals it. Zero
	// means no mint has reported one yet, and maxAge alone bounds the session.
	grantExpiresAt time.Time
	watchCancel    context.CancelFunc // cancels the live session's crash watcher on teardown
	launching      chan struct{}      // non-nil while an attestation is in flight (single-flight)
	cache          map[string]cachedToken
	negCache       map[string]negEntry // terminal player-context errors by video_id, guarded by mu

	// mu guards the streaming deadline, outstanding degradation report, and report
	// budget state. A suspect mark must not outlive its generation.
	streamingDeadline    time.Time
	reportSuspectGen     uint64
	reportSuspectVideoID string
	// reportTokens is the remaining report-driven recycle budget, a token bucket
	// refilled at one token per reportDebounce up to ReportBurst. reportRefillAt
	// is when refill last accrued.
	reportTokens   float64
	reportRefillAt time.Time

	// lastMintAt, lastProofAt, and lastEstablishAt are when the page last
	// completed an in-page mint, passed a full-length proof, and attempted any
	// playback (see markPlayback). They anchor the separation gates
	// (waitBeforeMint, waitBeforeEstablish) and describe one page, so publishing
	// a new generation clears them.
	lastMintAt      time.Time
	lastProofAt     time.Time
	lastEstablishAt time.Time

	// proofFailGen and proofFailedAt record the generation and time of the last
	// failed proof or bot check, so prove can refuse requests during the
	// cool-down without paying another proof attempt. A new generation or a
	// passing proof clears them.
	proofFailGen  uint64
	proofFailedAt time.Time
	// proofFailCause and proofFailCooldown are the failure that opened the record
	// and the window it earned (a bot check earns a longer one), so a refusal can
	// name the cause and state the right wait.
	proofFailCause    error
	proofFailCooldown time.Duration
	// proofCooldownWarnedAt is the proofFailedAt the cool-down warning last fired
	// for, so each window warns once rather than once per refusal. Cleared with
	// proofFailGen.
	proofCooldownWarnedAt time.Time

	// proofRelaunched marks that the current failure streak already spent its one
	// proof-driven relaunch. Relaunch backoff only rate-limits; this flag stops an
	// unprovable environment from restarting Chromium on every cool-down. It
	// survives new generations; only a successful proof (markProved) clears it.
	// Bot checks have their own window: see botCheckRelaunchedAt.
	proofRelaunched bool

	// botCheckRelaunchedAt is when a bot check last bought a fresh identity. It is
	// never cleared, markProved included: the grant is per window, not per streak,
	// so a replacement that proves and then walls on a context cannot claim a
	// second one.
	botCheckRelaunchedAt time.Time

	// retiredGen and retiredCrash record the last generation torn down and
	// whether the browser died under it, so sessionDied cannot mistake an
	// off-request recycle for a browser death. Only the newest teardown is kept.
	// Retirement runs in generation order, so a request from an older generation
	// has outlived a whole launch cycle and is treated as a death.
	retiredGen   uint64
	retiredCrash bool

	// shortGrantWarnedGen is the generation warnShortGrant last fired for, so it
	// logs once per generation rather than once per mint.
	shortGrantWarnedGen uint64

	// unrecognizedLoginWarned holds the reasons recordUnplayable has warned
	// about, at most unrecognizedLoginWarnMax. Guarded by mu.
	unrecognizedLoginWarned map[string]struct{}

	mintMu  sync.Mutex // serializes the page's mints, proofs, and contexts
	metrics minterMetrics
}

// minterSession is the part of browser.Session used by Minter. Tests replace it
// with an in-memory implementation.
type minterSession interface {
	Mint(ctx context.Context, identifier string) (browser.MintResult, error)
	PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, error)
	EnsureEstablished(ctx context.Context) error
	Ping(ctx context.Context) error
	AttestKind() string
	Identity() browser.Identity
	BrowserCookies(ctx context.Context) ([]*http.Cookie, error)
	Established() bool
	LastProof() (browser.FullLengthProbe, time.Time)
	Close()
}

type cachedToken struct {
	res    browser.MintResult
	expiry time.Time
	gen    uint64
}

// negEntry records a terminal player-context error and its expiry. It is not tied
// to a session generation because relaunching cannot make the video playable.
type negEntry struct {
	err    error
	expiry time.Time
}

// minterMetrics contains process-lifetime counters. Failure counters count
// attempts, not requests; the exceptions say so on their own field.
type minterMetrics struct {
	Attestations   atomic.Int64
	LaunchFailures atomic.Int64
	Mints          atomic.Int64
	MintFailures   atomic.Int64 // per attempt (see minterMetrics doc)
	// Escalations counts generations a request abandoned for a fresh one: a
	// ladder's second failure (mint, player-context, or proof) or a bot check,
	// whether the relaunch runs at once or falls to the next request. It counts
	// only when the request's own retire closed the generation; one retired out
	// from under it was not its to abandon.
	Escalations    atomic.Int64
	CacheHits      atomic.Int64
	CacheMisses    atomic.Int64
	CacheEvictions atomic.Int64 // positive-cache entries evicted at capacity
	// Crashes counts unexpected browser loss detected by CDP or a health probe.
	// Intentional session retirement does not count.
	Crashes atomic.Int64
	// ProbeFailures counts tenant probes that failed twice against a session no
	// request held, which /ping reports as probe-failed and operators alert on.
	// It counts even when the retirement that follows finds the generation gone.
	// Crashes counts those retirements too; this separates them from CDP deaths.
	// Once per request.
	ProbeFailures atomic.Int64
	// ProbeBusy counts probes that failed twice while a request held the page, so
	// nothing was retired (/ping reports busy). Busy stays HTTP 200, so without
	// this counter a daemon that is always busy looks healthy. Once per request.
	ProbeBusy             atomic.Int64
	PlayerContexts        atomic.Int64
	PlayerContextFailures atomic.Int64 // per failed attempt (see minterMetrics doc)
	// PlayerContextNegativeCacheHits counts requests refused from the negative
	// cache without touching the browser. They stay out of PlayerContextFailures
	// because one caller looping on an unplayable video could otherwise bury the
	// real failure rate. Once per request.
	PlayerContextNegativeCacheHits atomic.Int64
	// Status2Rejections counts refused requests where a player context could not
	// be confirmed beyond the status-2 cap. Attempts are counted by
	// PlayerContextFailures; this counter is once per request.
	Status2Rejections atomic.Int64
	// SeparationWaits counts each wait (not each request) that held an operation
	// back to keep an in-page mint and a context establishment mintSeparation
	// apart.
	SeparationWaits atomic.Int64
	// UnprovenRejections counts player-context and session requests refused
	// because no session proved full-length streaming: a failed proof, a
	// cool-down (after a failed proof or a bot check), or a browser that died
	// mid-proof. A death is not graded, but its refusal is still counted here,
	// since no other counter records it. Once per request.
	UnprovenRejections atomic.Int64
	// BotChecks counts each bot check the browser answered, at a proof or at a
	// context. A cool-down refusal that follows is counted under
	// UnprovenRejections, whichever failure opened the cool-down.
	BotChecks atomic.Int64
	// UnrecognizedLoginRefusals counts verdicts the browser flagged as a possible
	// rephrased bot wall (browser.UnplayableError.UnrecognizedLogin). They are
	// answered as unavailable but not negative-cached. Once per request, since
	// the verdict ends it.
	UnrecognizedLoginRefusals atomic.Int64

	// Session recycles are separated by cause.
	StreamingRecycles    atomic.Int64 // time-based recycle on a streaming handoff
	ReportDrivenRecycles atomic.Int64 // recycle triggered by a consumer degradation report

	// Consumer degradation reports, classified by disposition.
	DegradationReportsAccepted      atomic.Int64
	DegradationReportsRejectedStale atomic.Int64 // named an old or replaced generation
	DegradationReportsRateLimited   atomic.Int64 // rejected by the debounce
	// DegradationReportsAlreadyRetired counts reports naming the current
	// generation whose session was already retired by a crash or a prior report.
	// This is a benign no-op, not a stale report.
	DegradationReportsAlreadyRetired atomic.Int64
	// DegradationReportsDuplicatePending counts a repeat report for a generation
	// whose retirement is already pending. DegradationReportsAccepted counts only
	// the report that queued it.
	DegradationReportsDuplicatePending atomic.Int64
}

const (
	minterMaxCacheTTL   = 6 * time.Hour
	minterCacheMargin   = 5 * time.Minute // don't hand out a token within this of expiry
	minterDefaultMaxAge = 11 * time.Hour  // < the ~12h integrity lifetime
	minterNegCacheTTL   = 5 * time.Minute // remember an unplayable video_id this long
	minterNegCacheMax   = 256             // bound the negative cache
	// unrecognizedLoginWarnMax bounds the distinct reasons recordUnplayable warns
	// about per Minter; past it, a new reason is only counted.
	unrecognizedLoginWarnMax = 32
	// minterCacheMax bounds the positive token cache, larger than the negative
	// cache because entries are keyed by distinct video or visitor identities.
	// Per tenant that is about 0.8 MB of typical tokens, or 10 MB of unusually
	// large ones.
	minterCacheMax = 1024

	// DefaultReportDebounce is the report budget's refill interval: after a burst
	// of up to ReportBurst, report-driven re-attestation is held to a sustained 12
	// per hour.
	DefaultReportDebounce = 5 * time.Minute

	// ReportBurst is how many report-driven recycles may run back to back before
	// rate-limiting. It covers a well-behaved consumer's largest rotation: a
	// bulk-enumeration throttle escape retires its identity and re-asks up to
	// four times, and a decline mid-sequence makes it misread throttled entries
	// as gone. A budget drained by recent reports may not cover it.
	ReportBurst = 4

	// DefaultMintSeparation is how far apart the daemon keeps an in-page mint and
	// a context establishment (WAXSEAL_MINT_SEPARATION overrides it). 12 s clears
	// both measured edges. Mint: a context 0.6 s after the mint of the token that
	// streamed it was graded a preview 6 of 6 times, and ran full length 6 of 6 at
	// 10.6 s. Proof: a context 3 s after the proof ran full length 0 of 4 times,
	// and 4 of 4 at 12 s; a later sweep (WAXSEAL_E2E_AGING=3) ran 4 of 4 from 3 s
	// up, so that egress's edge was below 3 s, but the 3 s failures still stand.
	DefaultMintSeparation = 12 * time.Second

	// MintSeparationEnv overrides DefaultMintSeparation with a positive Go
	// duration, for example "20s".
	MintSeparationEnv = "WAXSEAL_MINT_SEPARATION"

	// MintSeparationWarn marks an override large enough that a first context has
	// to wait most of the way to the per-request budget before it is served.
	MintSeparationWarn = 60 * time.Second

	// proofRetryCooldown bounds how often a session that failed to prove pays
	// another proof attempt. A proof can hold mintMu for up to a minute, so
	// retrying on every request would stall every request behind a broken session
	// only to refuse it; the cool-down lets most of them fail fast.
	proofRetryCooldown = 30 * time.Second

	// botCheckCooldown is how long a bot check refuses before the same identity is
	// tried again. It is longer than the proof cool-down because a wall does not
	// lift in 30 s, and above 60 s because a consumer that sleeps through a stated
	// wait up to that would only retry into the same wall; above it, a consumer
	// reports at once and skips WEB for the wait, which is what a batch wants.
	botCheckCooldown = 2 * time.Minute

	// botCheckRelaunchWindow is how often a bot check may buy a fresh identity. A
	// passing proof does not reset it (see botCheckRelaunchedAt).
	botCheckRelaunchWindow = 10 * time.Minute

	// pingProbeTimeout allows for a busy host without leaving /ping unbounded.
	pingProbeTimeout = 5 * time.Second

	// A short retry window tolerates transient startup failures without hiding a
	// persistent minting failure.
	selfTestMintAttempts = 3
)

// selfTestMintRetryDelay is variable so tests can shorten the retry interval.
var selfTestMintRetryDelay = 1 * time.Second

// ErrNoSession reports that the tenant has no attested session, which callers
// such as server /ping treat as benign rather than as a probe failure.
var ErrNoSession = errors.New("waxseal: no attested session")

// ErrProbeBusy reports that a /ping probe failed twice while a request held the
// page, so nothing was retired. It says as much about contention as about the
// browser, so callers treat it as benign, unlike a failure that retired the
// session.
var ErrProbeBusy = errors.New("waxseal: ping probe failed while the session was busy")

// ErrClosed reports that the Minter is closed and will not launch another
// session. Only a request running past the server's shutdown drain sees it, so
// it maps to the same wire errors as an unavailable browser.
var ErrClosed = errors.New("waxseal: minter closed")

// ErrUnproven reports that the browser session could not prove full-length
// streaming, so nothing was handed out (see ensureProven). Callers use it to
// tell a refusal from an extraction failure.
var ErrUnproven = errors.New("waxseal: session has not proved full-length streaming")

// RetryAfterError wraps a refusal the daemon expects to lift on its own and says
// how long the caller should wait before asking again: the rest of a cool-down,
// or the browser pool's relaunch backoff. The server sends it as Retry-After and
// retry_after_seconds; errors.Is still sees the cause, so routing is unmoved.
type RetryAfterError struct {
	Err   error
	After time.Duration
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }

func (e *RetryAfterError) Unwrap() error { return e.Err }

// Seconds is After rounded up to whole seconds and never below 1, the form the
// header takes: a wait under a second is still later, not now.
func (e *RetryAfterError) Seconds() int { return max(1, ceilSeconds(e.After)) }

// unprovenRefusal is the refusal that starts a proof cool-down, so its wait is
// the whole window.
func unprovenRefusal(cause error) error {
	return &RetryAfterError{Err: fmt.Errorf("%w: %w", ErrUnproven, cause), After: proofRetryCooldown}
}

// botCheckRefusal is the refusal that starts a bot-check cool-down. The cause
// stays the bot check rather than ErrUnproven: the session may well have proved,
// and the refusal should name the wall.
func botCheckRefusal(cause error) error {
	return &RetryAfterError{Err: cause, After: botCheckCooldown}
}

// NewMinter builds a single-identity minter for video (the landing watch id). It
// launches a browser only when an operation first needs a session.
// streamingMaxAge forces a fresh session on the next streaming handoff once the
// current one is older (0 disables). reportDebounce is the report budget's
// refill interval (<=0 uses DefaultReportDebounce). A positive mintSeparation
// overrides the env-derived mint-to-establishment spacing (see
// resolveMintSeparation).
func NewMinter(video string, opts browser.Options, streamingMaxAge, reportDebounce, mintSeparation time.Duration) *Minter {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if reportDebounce <= 0 {
		reportDebounce = DefaultReportDebounce
	}
	m := &Minter{
		video:           video,
		opts:            opts,
		log:             log,
		maxAge:          minterDefaultMaxAge,
		streamingMaxAge: streamingMaxAge,
		reportDebounce:  reportDebounce,
		mintSeparation:  resolveMintSeparation(mintSeparation, log),
		reportTokens:    ReportBurst, // start with the full burst allowance
		reportRefillAt:  time.Now(),
		cache:           make(map[string]cachedToken),
		negCache:        make(map[string]negEntry),
	}
	m.launch = m.launchReal
	return m
}

// mintSeparationUnparseableOnce and mintSeparationLargeOnce log their warnings
// once per process, since every tenant that falls back to
// WAXSEAL_MINT_SEPARATION reads the same value.
var (
	mintSeparationUnparseableOnce sync.Once
	mintSeparationLargeOnce       sync.Once
)

// ParseMintSeparation reads a MintSeparationEnv value: blank is the default,
// anything else must be a positive Go duration. It is the single definition of
// what the variable accepts, so the server command can refuse a bad value at
// startup while a library caller keeps the lenient default below.
func ParseMintSeparation(raw string) (time.Duration, error) {
	if raw = strings.TrimSpace(raw); raw == "" {
		return DefaultMintSeparation, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid %s %q: want a positive Go duration such as 20s", MintSeparationEnv, raw)
	}
	return d, nil
}

// mintSeparationFromEnv reads the mint-to-establishment spacing for a caller
// that did not set one. A value ParseMintSeparation refuses keeps the default
// and logs why, once per process.
func mintSeparationFromEnv(log *slog.Logger) time.Duration {
	raw := os.Getenv(MintSeparationEnv)
	d, err := ParseMintSeparation(raw)
	if err != nil {
		mintSeparationUnparseableOnce.Do(func() {
			log.Warn("minter: ignoring "+MintSeparationEnv+"; want a positive Go duration such as 20s",
				"value", raw, "using", DefaultMintSeparation)
		})
		return DefaultMintSeparation
	}
	if d > MintSeparationWarn {
		mintSeparationLargeOnce.Do(func() {
			log.Warn("minter: "+MintSeparationEnv+" is large; values near the request budget make first contexts time out",
				"value", d)
		})
	}
	return d
}

// resolveMintSeparation returns mintSeparation when it is positive (an explicit
// override such as server.Config.MintSeparation) and otherwise the env-derived
// default, so WAXSEAL_MINT_SEPARATION works for callers that set no override.
func resolveMintSeparation(mintSeparation time.Duration, log *slog.Logger) time.Duration {
	if mintSeparation > 0 {
		return mintSeparation
	}
	return mintSeparationFromEnv(log)
}

// waitSeparation blocks until mintSeparation has passed since from, so in-page
// attestation work and a context establishment never land within that window of
// each other in either order. what and after name the held-back operation and
// the anchor, for the log line. A zero from (nothing recorded yet) or a
// non-positive separation returns at once.
//
// Callers hold mintMu for the whole wait, since releasing it would let other
// page work slip into the window and move the anchor. The wait is normally paid
// once per generation; meanwhile a cache-missing /get_pot queues behind it and
// a failing /ping reports busy instead of retiring.
func (m *Minter) waitSeparation(ctx context.Context, from time.Time, what, after string) error {
	if from.IsZero() || m.mintSeparation <= 0 {
		return nil
	}
	wait := m.mintSeparation - time.Since(from)
	if wait <= 0 {
		return nil
	}
	m.metrics.SeparationWaits.Add(1)
	m.log.Info("minter: separation wait", "what", what, "after", after, "wait", wait.Round(time.Millisecond))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// waitBeforeMint holds a mint back until it is mintSeparation clear of the last
// in-page playback attempt, successful or not (see markPlayback): a token
// minted right after an establishment is graded the same as one minted right
// before it.
func (m *Minter) waitBeforeMint(ctx context.Context) error {
	m.mu.Lock()
	establishAt := m.lastEstablishAt
	m.mu.Unlock()
	return m.waitSeparation(ctx, establishAt, "mint", "establishment")
}

// waitBeforeEstablish holds a context establishment back until it is
// mintSeparation clear of the later of the last mint and the last proof. An
// earlier context is not an anchor: a second context taken moments after the
// first streams full length. what names the caller.
func (m *Minter) waitBeforeEstablish(ctx context.Context, what string) error {
	m.mu.Lock()
	anchor, after := m.lastMintAt, "mint"
	if m.lastProofAt.After(anchor) {
		anchor, after = m.lastProofAt, "proof"
	}
	m.mu.Unlock()
	return m.waitSeparation(ctx, anchor, what, after)
}

// ensureProven makes the session prove full-length streaming before it hands out
// a context, since a context that is the session's first playback is graded a
// preview about as often as not. The proof runs once per session, normally in
// the startup self-test, and skips the separation window: only a context served
// to a consumer needs it. It returns the session and generation for the rest of
// the request; what ("player-context" or "session") labels its warn lines.
//
// A first failure opens proofRetryCooldown, refusing requests without another
// attempt; a second relaunches and proves a fresh session, once per failure
// streak (see proofRelaunched). Running out the request's deadline counts as a
// failure, so the next request is refused instead of spending its budget under
// mintMu; a canceled caller records nothing. A browser death mid-proof (see
// sessionDied) records nothing and spends no relaunch: the request proves the
// replacement once. A bot check walls the identity, so waiting cannot lift it:
// it relaunches at once, at most once per botCheckRelaunchWindow, and refuses
// for botCheckCooldown when that is spent or the replacement is walled too.
func (m *Minter) ensureProven(ctx context.Context, sess minterSession, gen uint64, what string) (minterSession, uint64, error) {
	return m.prove(ctx, sess, gen, what, true)
}

// prove is ensureProven's body. allowReplacement permits one replacement, for a
// death or a bot check during the proof; the replacement is proved with it
// false, so a request takes at most one replacement here.
func (m *Minter) prove(ctx context.Context, sess minterSession, gen uint64, what string, allowReplacement bool) (minterSession, uint64, error) {
	// Check the cool-down before the established short-circuit: a bot check can
	// open one on a session that already proved, which must be refused too.
	m.mu.Lock()
	onCooldown := m.proofFailGen == gen && time.Since(m.proofFailedAt) < m.proofFailCooldown
	var remaining time.Duration
	var cause error
	var warnCooldown bool
	if onCooldown {
		remaining = (m.proofFailCooldown - time.Since(m.proofFailedAt)).Round(time.Millisecond)
		cause = m.proofFailCause
		// Warn once per window (see proofCooldownWarnedAt).
		if m.proofCooldownWarnedAt != m.proofFailedAt {
			m.proofCooldownWarnedAt = m.proofFailedAt
			warnCooldown = true
		}
	}
	m.mu.Unlock()
	if onCooldown {
		// Name the failure that opened the window and state what is left of it.
		refusal := error(&RetryAfterError{Err: fmt.Errorf("%w: %w", ErrUnproven, cause), After: remaining})
		line := "minter: refusing " + what + "; session is in a proof cool-down"
		if errors.Is(cause, browser.ErrBotCheck) {
			line = "minter: refusing " + what + "; session is in a bot-check cool-down"
			refusal = &RetryAfterError{Err: cause, After: remaining}
		}
		if warnCooldown {
			m.log.Warn(line, "what", what, "gen", gen, "remaining", remaining)
		}
		m.log.Debug(line, "what", what, "gen", gen, "remaining", remaining)
		m.metrics.UnprovenRejections.Add(1)
		return sess, gen, refusal
	}
	if sess.Established() {
		return sess, gen, nil
	}

	proofErr := sess.EnsureEstablished(ctx)
	// Any playback attempt, successful or not, arms the mint gate: see markPlayback.
	m.markPlayback()
	if proofErr == nil {
		m.markProved()
		return sess, gen, nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return sess, gen, ctx.Err()
	}
	if m.sessionDied(sess, gen, what+" proof", proofErr) {
		// Out of budget to prove a replacement: refuse, recording nothing, since
		// the next request will not find the dead generation.
		if ctx.Err() != nil {
			m.metrics.UnprovenRejections.Add(1)
			return sess, gen, ctx.Err()
		}
		if allowReplacement {
			newSess, newGen, err := m.ensure(ctx)
			if err != nil {
				return nil, 0, err
			}
			return m.prove(ctx, newSess, newGen, what, false)
		}
		m.log.Warn("minter: refusing "+what+"; the replacement session died during its proof as well",
			"gen", gen, "err", proofErr)
		m.metrics.UnprovenRejections.Add(1)
		// No wait: nothing was recorded, and a quick retry is right after a death.
		return sess, gen, fmt.Errorf("%w: %w", ErrUnproven, proofErr)
	}
	deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)

	// A bot check skips the first-failure step (see ensureProven).
	if errors.Is(proofErr, browser.ErrBotCheck) {
		if !allowReplacement {
			// Already on this request's one replacement: open the cool-down and
			// refuse without claiming the window, as relaunchAndProve does.
			m.recordBotCheck(gen, proofErr, false)
			m.log.Warn("minter: refusing "+what+"; the replacement session hit a bot check as well",
				"gen", gen, "err", proofErr)
			m.metrics.UnprovenRejections.Add(1)
			if deadline {
				return sess, gen, ctx.Err()
			}
			return sess, gen, botCheckRefusal(proofErr)
		}
		// A caller whose own budget is already spent cannot launch and prove a fresh
		// session, so it does not claim the window either.
		if !m.recordBotCheck(gen, proofErr, !deadline) {
			m.log.Warn("minter: refusing "+what+"; hit a bot check with no relaunch to spend",
				"what", what, "gen", gen, "err", proofErr, "budget_spent", deadline)
			m.metrics.UnprovenRejections.Add(1)
			if deadline {
				return sess, gen, ctx.Err()
			}
			return sess, gen, botCheckRefusal(proofErr)
		}
		m.log.Warn("minter: "+what+" proof hit a bot check; relaunching for a fresh identity", "gen", gen, "err", proofErr)
		return m.relaunchAndProve(ctx, sess, gen, what, "bot check; relaunching", deadline)
	}

	m.log.Warn("minter: refusing "+what+"; session could not prove full-length streaming",
		"gen", gen, "err", proofErr)

	secondFailure, relaunch := m.recordProofFailure(gen, proofErr, true)
	if !secondFailure {
		m.metrics.UnprovenRejections.Add(1)
		if deadline {
			return sess, gen, ctx.Err()
		}
		return sess, gen, unprovenRefusal(proofErr)
	}
	if !relaunch {
		m.log.Warn("minter: proof-driven relaunch for this failure streak was already spent; the session will be recycled by a successful proof, a crash, a consumer report, or the max-age recycle",
			"what", what, "gen", gen)
		m.metrics.UnprovenRejections.Add(1)
		if deadline {
			return sess, gen, ctx.Err()
		}
		return sess, gen, unprovenRefusal(proofErr)
	}

	// A second failure, and recordProofFailure just claimed the streak's
	// relaunch: relaunch once and prove the fresh session.
	return m.relaunchAndProve(ctx, sess, gen, what, "proof failed twice; relaunching", deadline)
}

// relaunchAndProve is the tail both relaunching branches of prove share: retire
// gen for reason, launch a fresh session, and prove it. deadline says the
// caller's own budget is already spent. The caller has logged and recorded the
// failure that earned the relaunch, so the refusals below name the
// replacement's own. Escalations counts only if retire closed gen; the relaunch
// runs either way, since the request still needs a session it can prove.
func (m *Minter) relaunchAndProve(ctx context.Context, sess minterSession, gen uint64, what, reason string, deadline bool) (minterSession, uint64, error) {
	if deadline {
		// No budget left to launch and prove a fresh session: retire gen, leave
		// the relaunch to the next request's ensure, and refuse this one.
		m.metrics.UnprovenRejections.Add(1)
		if m.retire(gen, reason+" (out of budget; relaunching on the next request)", false) {
			m.metrics.Escalations.Add(1)
		}
		return sess, gen, ctx.Err()
	}
	if m.retire(gen, reason, false) {
		m.metrics.Escalations.Add(1)
	}
	newSess, newGen, err := m.ensure(ctx)
	if err != nil {
		// ensure counted the failed launch under launch_failures, so it is not
		// also an unproven rejection.
		return nil, 0, err
	}
	proofErr := newSess.EnsureEstablished(ctx)
	m.markPlayback()
	if proofErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return newSess, newGen, ctx.Err()
		}
		newDeadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
		// A replacement that died is refused as in prove, with nothing recorded.
		// Checking this first keeps the "could not prove" warn below off a crash,
		// which sessionDied's own log line would contradict.
		if m.sessionDied(newSess, newGen, what+" proof", proofErr) {
			m.metrics.UnprovenRejections.Add(1)
			if newDeadline {
				return newSess, newGen, ctx.Err()
			}
			m.log.Warn("minter: refusing "+what+"; the replacement session died during its proof as well",
				"gen", newGen, "err", proofErr)
			return newSess, newGen, fmt.Errorf("%w: %w", ErrUnproven, proofErr)
		}
		// A walled replacement counts as a bot check but claims no window: this
		// request is already on its one replacement.
		if errors.Is(proofErr, browser.ErrBotCheck) {
			m.log.Warn("minter: refusing "+what+"; the relaunched session hit a bot check as well",
				"gen", newGen, "err", proofErr)
			m.recordBotCheck(newGen, proofErr, false)
			m.metrics.UnprovenRejections.Add(1)
			if newDeadline {
				return newSess, newGen, ctx.Err()
			}
			return newSess, newGen, botCheckRefusal(proofErr)
		}
		m.log.Warn("minter: refusing "+what+"; relaunched session could not prove full-length streaming",
			"gen", newGen, "err", proofErr)
		// Record the fresh generation's failure without claiming the streak's
		// relaunch. A just-published generation has no prior record, so
		// secondFailure is always false; true would mean gen and newGen collided,
		// which deserves a warn rather than a silent second relaunch.
		if secondFailure, _ := m.recordProofFailure(newGen, proofErr, false); secondFailure {
			m.log.Warn("minter: relaunched session's generation already carried a proof-failure record; a freshly published generation should never carry one",
				"what", what, "gen", newGen)
		}
		// The failure on gen only triggered the relaunch, so this is the request's
		// one rejection.
		m.metrics.UnprovenRejections.Add(1)
		if newDeadline {
			return newSess, newGen, ctx.Err()
		}
		return newSess, newGen, unprovenRefusal(proofErr)
	}
	m.markProved()
	return newSess, newGen, nil
}

// recordProofFailure records a failed proof on gen under cause, which a
// cool-down refusal names, and reports whether it is gen's second failure and,
// if so, whether this call won the streak's one relaunch. A caller that must
// never relaunch passes claimRelaunch false: the self-test, or a check on a
// just-published generation. The grant is decided and claimed (proofRelaunched)
// in one critical section, so at most one caller gets it, and every caller
// holds mintMu, so no claim on another generation can race this one.
func (m *Minter) recordProofFailure(gen uint64, cause error, claimRelaunch bool) (secondFailure, relaunchGranted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The two rules share one cool-down record, so the streak reads the cause: a
	// bot check is not a proof failure and must not make this one the second.
	secondFailure = m.proofFailGen == gen && !errors.Is(m.proofFailCause, browser.ErrBotCheck)
	m.proofFailGen = gen
	m.proofFailedAt = time.Now()
	m.proofFailCause, m.proofFailCooldown = cause, proofRetryCooldown
	if secondFailure && claimRelaunch && !m.proofRelaunched {
		m.proofRelaunched = true
		relaunchGranted = true
	}
	return secondFailure, relaunchGranted
}

// recordBotCheck counts a bot check against gen, starts its cool-down, and
// reports whether it may relaunch: the first in botCheckRelaunchWindow does,
// and claims the window. There is no first-failure step, and the proof-failure
// streak is not consulted. A caller already on a replacement passes
// claimRelaunch false: it will not launch again, and claiming would spend the
// window with no fresh identity to show for it.
func (m *Minter) recordBotCheck(gen uint64, cause error, claimRelaunch bool) (relaunchGranted bool) {
	m.metrics.BotChecks.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proofFailGen, m.proofFailedAt = gen, time.Now()
	m.proofFailCause, m.proofFailCooldown = cause, botCheckCooldown
	if !claimRelaunch || time.Since(m.botCheckRelaunchedAt) < botCheckRelaunchWindow {
		return false
	}
	m.botCheckRelaunchedAt = time.Now()
	return true
}

// markMinted records a completed in-page mint.
func (m *Minter) markMinted() {
	m.mu.Lock()
	m.lastMintAt = time.Now()
	m.mu.Unlock()
}

// markPlayback records an in-page playback attempt, a proof or a context,
// successful or not; callers run it after every attempt. A failed attempt still
// touched the page, so it arms the mint gate (waitBeforeMint) like a successful
// one. A passing proof also calls markProved.
func (m *Minter) markPlayback() {
	m.mu.Lock()
	m.lastEstablishAt = time.Now()
	m.mu.Unlock()
}

// markProved records a passing full-length proof. A proof is also an
// establishment to the mint gate, so it sets both anchors; it also clears the
// cool-down record and ends the failure streak (see proofRelaunched).
func (m *Minter) markProved() {
	m.mu.Lock()
	now := time.Now()
	m.lastProofAt = now
	m.lastEstablishAt = now
	m.proofFailGen = 0
	m.proofFailedAt = time.Time{}
	m.proofCooldownWarnedAt = time.Time{}
	m.proofFailCause, m.proofFailCooldown = nil, 0
	m.proofRelaunched = false
	m.mu.Unlock()
}

// jitter varies d by up to 10 percent so a fleet of minters does not recycle in
// lockstep. Non-positive durations remain disabled.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

// launchReal starts a browser session and attests it.
func (m *Minter) launchReal(ctx context.Context) (minterSession, error) {
	sess, err := browser.Launch(ctx, m.video, m.opts)
	if err != nil {
		return nil, err
	}
	if err := sess.Attest(ctx); err != nil {
		sess.Close()
		return nil, err
	}
	return sess, nil
}

// Warm performs the single-flight attestation before the first request. It
// holds mintMu like every caller of ensure, so ensure's recycle branch cannot
// close an aged session under a request that holds it; running Warm before
// Serve is then a convenience, not a correctness condition.
func (m *Minter) Warm(ctx context.Context) error {
	m.mintMu.Lock()
	defer m.mintMu.Unlock()
	_, _, err := m.ensure(ctx)
	return err
}

// grantExhaustedLocked reports whether the generation's tokens are inside the
// cache margin of expiring. From then on the daemon would refuse to cache any
// token the session mints, however young the session is. A zero grant (no mint
// has reported an expiry) exhausts nothing. The caller must hold m.mu. Both
// recycle predicates use it; each keeps its own max-age check.
func (m *Minter) grantExhaustedLocked(now time.Time) bool {
	return !m.grantExpiresAt.IsZero() && !now.Before(m.grantExpiresAt.Add(-minterCacheMargin))
}

// grantWorthRecording reports whether a grant expires more than the cache
// margin after its attestation (not after now). A recycle renews a grant that
// ran down, but one inside the margin on arrival would recur, so recycling for
// it would silently tear down every new session under mintMu. Such a grant is
// dropped: its tokens stay uncacheable and maxAge alone bounds the session.
func grantWorthRecording(expiresAt, attestedAt time.Time) bool {
	return !expiresAt.IsZero() && expiresAt.After(attestedAt.Add(minterCacheMargin))
}

// recordGrantLocked latches gen's token expiry, measured against attestedAt,
// and reports a grant dropped for arriving inside the cache margin, once per
// generation, so the caller can warn after unlocking. The caller holds m.mu.
func (m *Minter) recordGrantLocked(gen uint64, attestedAt, expiresAt time.Time) (dropped bool) {
	if expiresAt.IsZero() {
		return false
	}
	if !grantWorthRecording(expiresAt, attestedAt) {
		if m.shortGrantWarnedGen == gen {
			return false
		}
		m.shortGrantWarnedGen = gen
		return true
	}
	m.grantExpiresAt = expiresAt
	return false
}

// warnShortGrant logs an attestation whose tokens were already inside the cache
// margin when issued. The session serves them uncached until maxAge recycles it
// (see grantWorthRecording).
func (m *Minter) warnShortGrant(gen uint64, expiresAt time.Time) {
	m.log.Warn("minter: attestation granted tokens that were already inside the cache margin; not bounding the session by them",
		"gen", gen, "expires_in", time.Until(expiresAt).Round(time.Second), "margin", minterCacheMargin)
}

// recordGrant records gen's token expiry, which bounds the session alongside
// maxAge. The first mint to report one fixes it (see grantExpiresAt). A zero
// expiry, or a mint from a generation the session has since left, records
// nothing, matching cachePut.
func (m *Minter) recordGrant(gen uint64, expiresAt time.Time) {
	if expiresAt.IsZero() {
		return
	}
	m.mu.Lock()
	if gen != m.gen || !m.grantExpiresAt.IsZero() {
		m.mu.Unlock()
		return
	}
	dropped := m.recordGrantLocked(gen, m.attestedAt, expiresAt)
	m.mu.Unlock()
	if dropped {
		m.warnShortGrant(gen, expiresAt)
	}
}

// sessionPastMaxAge reports whether the current generation is old enough, or its
// grant close enough to expiring, that ensure would recycle it. Mint's cache fast
// path checks it because the recycle, and the cache clear after it, live inside
// ensure. It keys on attestedAt: during a relaunch m.sess is nil but m.gen has
// not bumped, so keying on a live session would report fresh and serve the aged
// generation's entries. A zero attestedAt (never attested) is past no bound.
func (m *Minter) sessionPastMaxAge() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.grantExhaustedLocked(time.Now()) {
		return true
	}
	return !m.attestedAt.IsZero() && m.maxAge > 0 && time.Since(m.attestedAt) > m.maxAge
}

// recycleCauseLocked names why the live session should be recycled, or returns
// the empty string when it should not. "age" is an attestation older than maxAge;
// "grant" is a generation whose tokens are about to expire, which strands the
// session even while it is young. It keys on a live session because it decides
// whether to tear one down. The caller must hold m.mu.
func (m *Minter) recycleCauseLocked() string {
	switch {
	case m.sess == nil:
		return ""
	case m.maxAge > 0 && time.Since(m.attestedAt) > m.maxAge:
		return "age"
	case m.grantExhaustedLocked(time.Now()):
		return "grant"
	default:
		return ""
	}
}

// ensure returns the live session and its generation. Concurrent launches
// coalesce into one attestation, and a session older than maxAge or past its
// grant is recycled.
func (m *Minter) ensure(ctx context.Context) (minterSession, uint64, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, 0, ErrClosed
		}
		// This path bypasses retire, so it must also clear the suspect mark.
		if cause := m.recycleCauseLocked(); cause != "" {
			old, gen, age := m.sess, m.gen, time.Since(m.attestedAt)
			m.sess = nil
			cancel := m.watchCancel
			m.watchCancel = nil
			m.dropReportedGenerationLocked(gen)
			m.reportSuspectGen = 0
			m.reportSuspectVideoID = ""
			// Record a non-crash, so a request holding this generation does not
			// read its failure as a browser death (see sessionDied).
			m.retiredGen, m.retiredCrash = gen, false
			m.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			m.log.Info("minter: session recycle", "gen", gen, "age", age.Round(time.Second), "cause", cause)
			old.Close()
			continue
		}
		if m.sess != nil {
			s, g := m.sess, m.gen
			m.mu.Unlock()
			return s, g, nil
		}
		if m.launching != nil { // another goroutine is attesting; wait for it.
			ch := m.launching
			m.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
			continue
		}
		// We own the (single-flighted) launch.
		ch := make(chan struct{})
		m.launching = ch
		m.mu.Unlock()

		sess, err := m.launch(ctx)

		// Mint the visitor's GVS token before publishing the session, while no
		// other call can reach the page. It puts the token a consumer streams with
		// well ahead of the first context establishment, the spacing
		// waitSeparation then keeps. A failure is not fatal: the session is
		// published anyway and the next token request mints on demand.
		var premint browser.MintResult
		var premintBinding string
		preminted := false
		if err == nil && !m.skipPremint {
			premintBinding = sess.Identity().VisitorData
			res, mintErr := sess.Mint(ctx, premintBinding)
			switch {
			case mintErr == nil:
				m.metrics.Mints.Add(1)
				premint, preminted = res, true
			case ctx.Err() != nil:
				// A canceled or timed-out launch is not a mint failure, matching Mint.
				m.log.Warn("minter: startup mint abandoned", "err", mintErr)
			default:
				m.metrics.MintFailures.Add(1)
				m.log.Warn("minter: startup mint failed; the next token request mints on demand", "err", mintErr)
			}
		}

		var shortGrant bool
		m.mu.Lock()
		m.launching = nil
		close(ch)
		if err != nil {
			m.mu.Unlock()
			m.metrics.LaunchFailures.Add(1)
			// Pass the pool's remaining backoff on as the caller's wait.
			if be, ok := errors.AsType[*browser.RelaunchBackoffError](err); ok {
				err = &RetryAfterError{Err: err, After: be.Wait}
			}
			return nil, 0, err
		}
		if m.closed {
			// Close landed during the launch, the one way a session can still
			// reach publication after Close. Discard it rather than leave the pool
			// a browser context nothing will close; waiters released by close(ch)
			// hit the closed check at the top.
			m.mu.Unlock()
			// Log it, or a launch that attested, maybe pre-minted, and produced
			// nothing leaves a gap between the counters and the session log.
			m.log.Info("minter: discarding a session that finished launching after Close", "preminted", preminted)
			sess.Close()
			return nil, 0, ErrClosed
		}
		m.sess = sess
		m.gen++
		// Fixed before the pre-mint's grant is recorded, which is measured against it.
		m.attestedAt = time.Now()
		// A new page starts with no marks and no proof-failure record. A
		// successful pre-mint sets its first mark below.
		m.lastMintAt = time.Time{}
		m.lastProofAt = time.Time{}
		m.lastEstablishAt = time.Time{}
		m.grantExpiresAt = time.Time{}
		m.proofFailGen = 0
		m.proofFailedAt = time.Time{}
		m.proofCooldownWarnedAt = time.Time{}
		m.proofFailCause, m.proofFailCooldown = nil, 0
		// A generation bump invalidates every cached token; under m.mu no
		// new-generation cachePut can precede the clear. The negative cache stays:
		// an unplayable video remains unplayable across a relaunch.
		clear(m.cache)
		if preminted {
			m.lastMintAt = time.Now()
			// The pre-mint's expiry bounds the session alongside maxAge. A result
			// without one leaves that to a later mint, and one already inside the
			// cache margin is dropped (see grantWorthRecording).
			shortGrant = m.recordGrantLocked(m.gen, m.attestedAt, premint.ExpiresAt)
			// Cache the pre-mint under both gvs and the default pot scope: scope
			// only namespaces the cache, and the token is the same either way.
			// Writing it in the critical section that publishes m.sess means no
			// caller sees the session without its token.
			m.cachePutLocked(cacheKey("gvs", premintBinding), premint, m.gen)
			m.cachePutLocked(cacheKey("pot", premintBinding), premint, m.gen)
		}
		// Arm the streaming deadline for this generation.
		if m.streamingMaxAge > 0 {
			m.streamingDeadline = time.Now().Add(jitter(m.streamingMaxAge))
		}
		g := m.gen
		// The crash watcher must outlive the (transient) launch ctx, so give it a
		// session-scoped context canceled only when this session is torn down.
		watchCtx, cancel := context.WithCancel(context.Background())
		m.watchCancel = cancel
		m.mu.Unlock()
		m.metrics.Attestations.Add(1)
		if shortGrant {
			m.warnShortGrant(g, premint.ExpiresAt)
		}
		m.log.Info("minter: session ready", "gen", g, "attest", sess.AttestKind())
		go m.watchCrash(sess, watchCtx, g)
		return sess, g, nil
	}
}

// retire closes generation gen if it is current and reports whether it closed a
// session. It also clears any degradation report for that generation. If isCrash
// is true, it increments Crashes. The generation check makes concurrent
// retirement attempts idempotent.
func (m *Minter) retire(gen uint64, reason string, isCrash bool) bool {
	m.mu.Lock()
	if m.sess == nil || m.gen != gen {
		m.mu.Unlock()
		return false
	}
	old := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.dropReportedGenerationLocked(gen)
	m.reportSuspectGen = 0
	m.reportSuspectVideoID = ""
	m.retiredGen, m.retiredCrash = gen, isCrash
	if isCrash {
		m.metrics.Crashes.Add(1)
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.log.Warn("minter: retiring session", "gen", gen, "reason", reason)
	old.Close()
	return true
}

// watchCrash retires the session when its browser target crashes, detaches, or
// loses the CDP connection, so the next request relaunches instead of failing
// against a dead page, and a request holding the session takes the replacement
// (see sessionDied). ctx lives as long as the session, not the launch request.
// Only browser.Session exposes the event stream; test fakes are ignored.
func (m *Minter) watchCrash(s minterSession, ctx context.Context, gen uint64) {
	real, ok := s.(*browser.Session)
	if !ok {
		return
	}
	reason := real.WaitCrash(ctx)
	// Intentional retirement cancels the watcher before closing the session.
	if ctx.Err() != nil {
		return
	}
	if reason == "" {
		return // no crash detected (e.g. the session had no live page)
	}
	m.retire(gen, reason, true)
}

// retireReported retires the current generation when a consumer report is
// pending on it, charging the report budget the way an immediate retirement
// does. Every request that takes the page calls it first, so a deferred report
// is consumed by the next request of any kind. The caller holds mintMu.
func (m *Minter) retireReported() bool {
	m.mu.Lock()
	cur := m.gen
	suspect := m.sess != nil && m.reportSuspectGen == cur && cur != 0
	m.mu.Unlock()
	if !suspect || !m.retire(cur, "consumer reported degradation; relaunching", false) {
		return false
	}
	m.metrics.ReportDrivenRecycles.Add(1)
	// Deferred and immediate report-driven recycles share one budget.
	m.mu.Lock()
	m.spendReportTokenLocked()
	m.mu.Unlock()
	return true
}

// refreshStreamingSession replaces a stale or reported-degraded session before a
// streaming handoff. The caller must hold mintMu. Token-only requests do not
// recycle an otherwise usable session for age, though they do consume a pending
// report the same way this does.
func (m *Minter) refreshStreamingSession(ctx context.Context) (minterSession, uint64, error) {
	if !m.retireReported() {
		m.mu.Lock()
		cur := m.gen
		stale := m.sess != nil && !m.streamingDeadline.IsZero() && time.Now().After(m.streamingDeadline)
		m.mu.Unlock()
		// retire verifies that cur is still current.
		if stale && m.retire(cur, "streaming session exceeded max age; relaunching", false) {
			m.metrics.StreamingRecycles.Add(1)
		}
	}
	return m.ensure(ctx)
}

// Generation returns the current session generation, or 0 before the first
// attestation. A consumer can pass it to ReportDegraded to name the exact session
// that produced a degraded context.
func (m *Minter) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

// ReportResult describes how the minter handled a degradation report. Accepted
// means the report applies to the current session; Retired, that the session
// was closed at once; RetirementPending, that the next request to take the page
// will close it. Accepted with neither means a crash or recycle closed the
// session first. RetryAfterSeconds is set when the report was rate-limited.
type ReportResult struct {
	Accepted          bool
	Retired           bool
	RetirementPending bool
	Generation        uint64
	RetryAfterSeconds int
}

// ReportDegraded records that generation gen produced a degraded stream. videoID
// and reason are diagnostic. The report is rate-limited and applies only to the
// current generation. Its cached tokens are dropped at once either way; if a
// browser operation is in progress, retirement is deferred to the next request
// that takes the page.
func (m *Minter) ReportDegraded(gen uint64, videoID, reason string) ReportResult {
	// Marking the generation before releasing m.mu deduplicates concurrent reports.
	m.mu.Lock()
	cur := m.gen
	m.refillReportTokensLocked(time.Now())
	switch {
	case gen != cur:
		// An old or future generation: already replaced, or never existed.
		m.mu.Unlock()
		m.metrics.DegradationReportsRejectedStale.Add(1)
		return ReportResult{Accepted: false, Generation: cur}
	case m.sess == nil:
		// The current generation, already retired (by a crash or a prior report)
		// before the next request relaunches: a benign no-op, not a stale report.
		// It must precede the rate-limit case: a report-driven retire leaves gen
		// unchanged and spends budget, so a re-report can match both.
		// TestMinterReportDegradedAlreadyRetired pins the order.
		m.mu.Unlock()
		m.metrics.DegradationReportsAlreadyRetired.Add(1)
		return ReportResult{Accepted: false, Generation: cur}
	case m.reportSuspectGen == gen:
		// Retirement is already pending.
		m.mu.Unlock()
		m.metrics.DegradationReportsDuplicatePending.Add(1)
		return ReportResult{Accepted: true, RetirementPending: true, Generation: gen}
	case m.reportTokens < 1:
		// Budget spent: tell the consumer how long until one token refills.
		retryAfter := ceilSeconds(time.Duration((1 - m.reportTokens) * float64(m.reportDebounce)))
		m.mu.Unlock()
		m.metrics.DegradationReportsRateLimited.Add(1)
		return ReportResult{Accepted: false, Generation: cur, RetryAfterSeconds: retryAfter}
	}
	m.reportSuspectGen = gen
	m.reportSuspectVideoID = videoID
	// Drop the tokens now: a deferred retirement would otherwise leave them
	// servable until the page frees up.
	m.dropReportedGenerationLocked(gen)
	m.metrics.DegradationReportsAccepted.Add(1)
	m.mu.Unlock()

	retireReason := "consumer report"
	if reason != "" {
		retireReason += ": " + reason
	}
	// Only the first report for this generation attempts immediate retirement.
	if m.mintMu.TryLock() {
		acted := m.retire(gen, retireReason, false)
		if acted {
			m.mu.Lock()
			m.spendReportTokenLocked()
			m.mu.Unlock()
			m.metrics.ReportDrivenRecycles.Add(1)
		} else {
			m.log.Debug("minter: report accepted but nothing was left to retire; a crash or recycle landed first", "gen", gen)
		}
		m.mintMu.Unlock()
		return ReportResult{Accepted: true, Retired: acted, Generation: gen}
	}
	// A browser operation holds mintMu; the next request to take the page retires
	// the session.
	return ReportResult{Accepted: true, RetirementPending: true, Generation: gen}
}

// ceilSeconds rounds a duration up to whole seconds.
func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// refillReportTokensLocked accrues report budget at one token per
// reportDebounce, capped at ReportBurst. The caller must hold m.mu.
func (m *Minter) refillReportTokensLocked(now time.Time) {
	elapsed := now.Sub(m.reportRefillAt)
	if elapsed <= 0 {
		return
	}
	m.reportTokens = min(m.reportTokens+float64(elapsed)/float64(m.reportDebounce), ReportBurst)
	m.reportRefillAt = now
}

// spendReportTokenLocked consumes one token of report budget. It is called only
// after a report-driven retire recycled the session, so a no-op retire (a crash
// or recycle got there first) never spends budget. The caller must hold m.mu.
func (m *Minter) spendReportTokenLocked() {
	m.refillReportTokensLocked(time.Now())
	m.reportTokens = max(m.reportTokens-1, 0)
}

// Mint returns a token for (scope, binding), reporting whether it came from cache.
// It serves a cached token first, retries one failed mint in place, then
// relaunches and attests before the final attempt.
func (m *Minter) Mint(ctx context.Context, scope, binding string) (res browser.MintResult, cached bool, err error) {
	key := cacheKey(scope, binding)
	// An aged session skips the cache so the request reaches ensure, which
	// recycles it; otherwise a cache-hit-only workload would serve it forever.
	if !m.sessionPastMaxAge() {
		if r, ok := m.cacheGet(key); ok {
			m.metrics.CacheHits.Add(1)
			return r, true, nil
		}
	}

	m.mintMu.Lock() // one page, so mints serialize
	defer m.mintMu.Unlock()
	// Taking the page consumes a pending report, so no fresh token comes from a
	// session the consumer called degraded.
	m.retireReported()
	// Recheck after waiting for mintMu, which can take as long as a full-length
	// proof: another goroutine may have filled the cache, or the session may have
	// aged past its bound.
	if !m.sessionPastMaxAge() {
		if r, ok := m.cacheGet(key); ok {
			m.metrics.CacheHits.Add(1)
			return r, true, nil
		}
	}

	sess, gen, err := m.ensure(ctx)
	if err != nil {
		// ensure could not replace the aged session. Its cached token is still
		// within its own expiry and generation (a failed launch publishes no
		// generation and clears nothing), so serve it rather than 502 every
		// request. A reported generation's tokens were dropped on arrival, so a
		// report followed by a failed relaunch fails closed; ReportBurst and the
		// debounce bound how often a consumer can cause that.
		if ctx.Err() == nil {
			if r, ok := m.cacheGet(key); ok {
				m.metrics.CacheHits.Add(1)
				m.log.Warn("minter: serving a cached token from the aged generation; its replacement could not be launched", "err", err)
				return r, true, nil
			}
		}
		return browser.MintResult{}, false, err
	}
	// ensure's pre-mint may have just filled this entry, so check again before
	// minting a redundant token. No age gate: ensure just returned a generation
	// within its bound.
	if r, ok := m.cacheGet(key); ok {
		m.metrics.CacheHits.Add(1)
		return r, true, nil
	}
	// Count the miss only after the rechecks, so a request served by the
	// pre-mint counts as a hit alone and a failed launch only under
	// launch_failures.
	m.metrics.CacheMisses.Add(1)
	if err := m.waitBeforeMint(ctx); err != nil {
		return browser.MintResult{}, false, err
	}
	res, err = sess.Mint(ctx, binding)
	// level 1: a live session's transient failure, one in-place retry, no
	// re-attest. A canceled or timed-out caller and a browser that died under
	// the request skip it. The block below returns the context error without
	// touching failure metrics or the ladder (as playerContextStop and Health
	// do, leaving a stuck page to the crash watcher or /ping), or takes the
	// replacement.
	if err != nil && ctx.Err() == nil && !m.isSuperseded(sess, gen) {
		m.metrics.MintFailures.Add(1)
		m.log.Warn("minter: mint failed; retrying on same session", "gen", gen, "err", err)
		if err := m.waitBeforeMint(ctx); err != nil {
			return browser.MintResult{}, false, err
		}
		res, err = sess.Mint(ctx, binding)
	}
	if err != nil {
		if ctx.Err() != nil {
			return browser.MintResult{}, false, ctx.Err()
		}
		// level 2: a live session failed twice; escalate to a relaunch and
		// re-attest on a fresh session. A session that died under the request is
		// already retired and graded nowhere: it goes straight to the replacement.
		if !m.sessionDied(sess, gen, "mint", err) {
			m.metrics.MintFailures.Add(1)
			// Only a retire that closed gen escalates (see Escalations). With
			// mintMu held, only Close or the crash watcher can retire gen first.
			if m.retire(gen, "mint failed twice; relaunching", false) {
				m.metrics.Escalations.Add(1)
			}
		}
		res, gen, err = m.mintOnReplacement(ctx, binding)
		if err != nil {
			return browser.MintResult{}, false, err
		}
	}
	m.metrics.Mints.Add(1)
	m.markMinted()
	m.recordGrant(gen, res.ExpiresAt)
	m.cachePut(key, res, gen)
	return res, false, nil
}

// mintOnReplacement mints binding on the session ensure publishes next, for a
// request whose own session is gone (retired by Mint's ladder, or dead). There
// is no cache recheck: the relaunch's pre-mint binds the fresh identity's
// visitor data, never the caller's binding.
func (m *Minter) mintOnReplacement(ctx context.Context, binding string) (browser.MintResult, uint64, error) {
	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return browser.MintResult{}, 0, err
	}
	res, err := sess.Mint(ctx, binding)
	if err != nil {
		if ctx.Err() != nil {
			return browser.MintResult{}, 0, ctx.Err()
		}
		m.metrics.MintFailures.Add(1)
		return browser.MintResult{}, 0, fmt.Errorf("minter: mint failed after relaunch: %w", err)
	}
	return res, gen, nil
}

// PlayerContext returns the attested browser's streaming context for videoID. It
// reuses the warm session and follows the same retry and relaunch policy as Mint.
// Successful contexts are not cached because their URLs contain a short-lived
// nonce. Terminal unplayable errors are cached briefly (see recordUnplayable).
func (m *Minter) PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, uint64, error) {
	// A known-unplayable video fails before mintMu and the session, so a consumer
	// retrying a 502 (or a malicious caller) cannot force repeated relaunches.
	if err := m.negCacheGet(videoID); err != nil {
		m.metrics.PlayerContextNegativeCacheHits.Add(1)
		m.log.Debug("minter: player-context refused from the negative cache", "video_id", videoID)
		return browser.PlayerContext{}, 0, err
	}

	m.mintMu.Lock() // one page, so player-context calls serialize with mints
	defer m.mintMu.Unlock()

	sess, gen, err := m.refreshStreamingSession(ctx)
	if err != nil {
		return browser.PlayerContext{}, 0, err
	}

	// Prove the session, then keep the context clear of the last mint or proof.
	// Both follow refreshStreamingSession, which may have relaunched and
	// pre-minted, and ensureProven may relaunch too, so use the pair it returns.
	sess, gen, err = m.ensureProven(ctx, sess, gen, "player-context")
	if err != nil {
		return browser.PlayerContext{}, gen, err
	}
	if err := m.waitBeforeEstablish(ctx, "player-context"); err != nil {
		return browser.PlayerContext{}, gen, err
	}
	pc, err := sess.PlayerContext(ctx, videoID)
	m.markPlayback() // any attempt, successful or not, arms the mint gate.
	if err == nil {
		m.metrics.PlayerContexts.Add(1)
		return pc, gen, nil
	}
	// A browser that died under the request is already retired, and its failure
	// is not counted, retried, or escalated. A departed caller gets no
	// replacement (see playerContextDied).
	if m.playerContextDied(ctx, sess, gen, err) {
		return m.playerContextOnReplacement(ctx, videoID)
	}
	// A bot check is about the identity, not the video and not the page, so it
	// skips the in-place retry that would meet the same wall.
	if ctx.Err() == nil && errors.Is(err, browser.ErrBotCheck) {
		return m.playerContextBotChecked(ctx, videoID, gen, err)
	}
	if m.playerContextStop(ctx, videoID, err) { // terminal or canceled: don't escalate.
		return browser.PlayerContext{}, gen, err
	}

	// level 1: transient failure, one in-place retry, no re-attest. A generation
	// retired out from under this request skips it, since the retry would call a
	// page that is already closed. A death has already taken the replacement
	// above, so only a deliberate retirement reaches this skip.
	if !m.isSuperseded(sess, gen) {
		m.log.Warn("minter: player-context failed; retrying on same session", "gen", gen, "err", err)
		if err := m.waitBeforeEstablish(ctx, "player-context"); err != nil {
			return browser.PlayerContext{}, gen, err
		}
		pc, err = sess.PlayerContext(ctx, videoID)
		m.markPlayback()
		if err == nil {
			m.metrics.PlayerContexts.Add(1)
			return pc, gen, nil
		}
		if m.playerContextDied(ctx, sess, gen, err) {
			return m.playerContextOnReplacement(ctx, videoID)
		}
		if ctx.Err() == nil && errors.Is(err, browser.ErrBotCheck) {
			return m.playerContextBotChecked(ctx, videoID, gen, err)
		}
		if m.playerContextStop(ctx, videoID, err) {
			return browser.PlayerContext{}, gen, err
		}
	}

	// A status-2 confirmation failure is timing, not a dead session: refuse it
	// without relaunching, even when the retry was skipped, since a relaunch
	// under load tends to make it worse. playerContextStop already counted each
	// attempt.
	if errors.Is(err, browser.ErrStatus2Unconfirmed) {
		m.metrics.Status2Rejections.Add(1)
		return browser.PlayerContext{}, gen, err
	}

	// An incomplete context is a session-local, often transient extraction miss,
	// and a relaunch (under mintMu) cannot fix a player response that lacks the
	// data. Return it without relaunching or negative-caching; the next request
	// can retry. playerContextStop already counted each attempt.
	if errors.Is(err, browser.ErrIncompleteContext) {
		return browser.PlayerContext{}, gen, err
	}

	// level 2: relaunch and re-attest on a fresh session (see Escalations).
	if m.retire(gen, "player-context failed twice; relaunching", false) {
		m.metrics.Escalations.Add(1)
	}
	return m.playerContextOnReplacement(ctx, videoID)
}

// playerContextBotChecked answers a bot check met while serving a context. The
// wall keys on the visitor, so only a fresh identity lifts it: the request
// relaunches inline, as level 2 does, and is served from the replacement. A
// window whose relaunch is spent refuses with the cool-down instead. The video
// is never negative-cached; nothing is wrong with it.
func (m *Minter) playerContextBotChecked(ctx context.Context, videoID string, gen uint64, err error) (browser.PlayerContext, uint64, error) {
	m.metrics.PlayerContextFailures.Add(1)
	if !m.recordBotCheck(gen, err, true) {
		m.log.Warn("minter: refusing player-context; hit a bot check with no relaunch to spend",
			"gen", gen, "video_id", videoID, "err", err)
		return browser.PlayerContext{}, gen, botCheckRefusal(err)
	}
	m.log.Warn("minter: player-context hit a bot check; relaunching for a fresh identity",
		"gen", gen, "video_id", videoID, "err", err)
	if m.retire(gen, "bot check; relaunching", false) {
		m.metrics.Escalations.Add(1)
	}
	return m.playerContextOnReplacement(ctx, videoID)
}

// playerContextOnReplacement serves videoID from the session ensure publishes
// next, for a request whose own session is gone (retired by PlayerContext's
// ladder, or dead). The replacement has never played and was pre-minted, so it
// needs the same proof and spacing as the attempts before it.
func (m *Minter) playerContextOnReplacement(ctx context.Context, videoID string) (browser.PlayerContext, uint64, error) {
	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return browser.PlayerContext{}, 0, err
	}
	// This is already the request's one extra session, so its proof gets no
	// replacement for a death or a bot check: ensureProven would otherwise
	// launch a third session under mintMu for one request, blocking the tenant,
	// with none of it counted.
	sess, gen, err = m.prove(ctx, sess, gen, "player-context", false)
	if err != nil {
		return browser.PlayerContext{}, gen, err
	}
	if err := m.waitBeforeEstablish(ctx, "player-context"); err != nil {
		return browser.PlayerContext{}, gen, err
	}
	pc, err := sess.PlayerContext(ctx, videoID)
	m.markPlayback()
	if err != nil {
		// A departed or timed-out caller is not a player-context failure, as in
		// playerContextStop and mintOnReplacement.
		if ctx.Err() != nil {
			return browser.PlayerContext{}, gen, ctx.Err()
		}
		m.metrics.PlayerContextFailures.Add(1)
		if errors.Is(err, browser.ErrUnplayable) {
			m.recordUnplayable(videoID, err)
			return browser.PlayerContext{}, gen, err
		}
		// A walled replacement opens the cool-down but claims no window: this
		// request is already on its one replacement.
		if errors.Is(err, browser.ErrBotCheck) {
			m.recordBotCheck(gen, err, false)
			m.log.Warn("minter: refusing player-context; the replacement session hit a bot check as well",
				"gen", gen, "video_id", videoID, "err", err)
			return browser.PlayerContext{}, gen, botCheckRefusal(err)
		}
		return browser.PlayerContext{}, gen, fmt.Errorf("minter: player-context failed after relaunch: %w", err)
	}
	m.metrics.PlayerContexts.Add(1)
	return pc, gen, nil
}

// playerContextDied reports whether the ladder should hand this attempt the
// replacement session. A departed caller is not owed one, and neither is a
// status-2 confirmation failure on a live session: that is timing, refused
// without a relaunch. The live qualifier matters because a browser that dies
// mid-confirm also surfaces as ErrStatus2Unconfirmed (the confirm loop retries
// eval errors until its budget runs out); once the session is gone, sessionDied
// decides, so a crash is served from the replacement.
func (m *Minter) playerContextDied(ctx context.Context, sess minterSession, gen uint64, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, browser.ErrStatus2Unconfirmed) && !m.isSuperseded(sess, gen) {
		return false
	}
	return m.sessionDied(sess, gen, "player-context", err)
}

// playerContextStop records a failed attempt and reports whether retries should
// stop. Terminal unplayable errors are recorded (see recordUnplayable), and
// canceled requests never cause a relaunch.
func (m *Minter) playerContextStop(ctx context.Context, videoID string, err error) bool {
	// A canceled or timed-out caller is not a player-context failure: stop
	// without counting or negative-caching it, as Mint does.
	if ctx.Err() != nil {
		return true
	}
	m.metrics.PlayerContextFailures.Add(1)
	if errors.Is(err, browser.ErrUnplayable) {
		m.recordUnplayable(videoID, err)
		return true
	}
	return false
}

// recordUnplayable negative-caches a terminal verdict for videoID, unless the
// browser flagged it as a possible rephrased bot wall
// (browser.UnplayableError.UnrecognizedLogin): caching a wall would stick it to
// the video. That verdict is counted instead, and each distinct reason warns
// once per Minter.
func (m *Minter) recordUnplayable(videoID string, err error) {
	ue, ok := errors.AsType[*browser.UnplayableError](err)
	if !ok || !ue.UnrecognizedLogin {
		m.negCachePut(videoID, err)
		return
	}
	m.metrics.UnrecognizedLoginRefusals.Add(1)
	m.mu.Lock()
	_, seen := m.unrecognizedLoginWarned[ue.Detail]
	warn := !seen && len(m.unrecognizedLoginWarned) < unrecognizedLoginWarnMax
	if warn {
		if m.unrecognizedLoginWarned == nil {
			m.unrecognizedLoginWarned = make(map[string]struct{})
		}
		m.unrecognizedLoginWarned[ue.Detail] = struct{}{}
	}
	m.mu.Unlock()
	if warn {
		m.log.Warn("minter: LOGIN_REQUIRED refusal with no video details and an unrecognized reason, not cached; if YouTube rephrased its bot wall, the phrase browser.isBotCheck matches needs updating",
			"reason", ue.Detail, "video_id", videoID)
	}
}

// negCacheGet returns a cached terminal error for videoID until its TTL expires.
func (m *Minter) negCacheGet(videoID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.negCache[videoID]
	if !ok {
		return nil
	}
	if time.Now().After(e.expiry) {
		delete(m.negCache, videoID)
		return nil
	}
	return e.err
}

// negCachePut remembers a terminal error for videoID for a short TTL. It removes
// expired entries first and evicts an arbitrary live entry if the map remains
// full.
func (m *Minter) negCachePut(videoID string, err error) {
	m.mu.Lock()
	if m.closed { // see cachePutLocked: Close leaves both caches empty
		m.mu.Unlock()
		return
	}
	now := time.Now()
	// Only a new key can force eviction; refreshing an existing entry must not drop
	// another, matching cachePut.
	if _, exists := m.negCache[videoID]; !exists && len(m.negCache) >= minterNegCacheMax {
		for k, e := range m.negCache {
			if now.After(e.expiry) {
				delete(m.negCache, k)
			}
		}
		if len(m.negCache) >= minterNegCacheMax { // all live: evict one to make room
			for k := range m.negCache {
				delete(m.negCache, k)
				break
			}
		}
	}
	m.negCache[videoID] = negEntry{err: err, expiry: now.Add(minterNegCacheTTL)}
	m.mu.Unlock()

	// The one place a fresh verdict enters the cache, so it fires once per
	// verdict rather than once per request from a caller looping on one video.
	// Logged outside m.mu, as retire does, so a slow handler cannot hold it.
	status := ""
	if ue, ok := errors.AsType[*browser.UnplayableError](err); ok {
		status = ue.Status
	}
	m.log.Info("minter: video unavailable; verdict cached", "video_id_len", len(videoID), "status", status)
	// A distinct message, not a second copy of the line above: -v would
	// otherwise report two verdicts for every one cached.
	m.log.Debug("minter: negative cache entry written", "video_id", videoID, "status", status, "err", err)
}

// cacheKey returns the shared key format used by request and startup mints.
func cacheKey(scope, binding string) string { return scope + "|" + binding }

func (m *Minter) cacheGet(key string) (browser.MintResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cache[key]
	if !ok || c.gen != m.gen || time.Now().After(c.expiry) {
		if ok {
			// A generation mismatch should be rare (ensure clears the cache and
			// cachePut rejects stale writes). Delete any unusable entry found here
			// so expired tokens do not sit in the map until the next generation.
			delete(m.cache, key)
		}
		return browser.MintResult{}, false
	}
	return c.res, true
}

// servableCacheEntriesLocked counts the entries cacheGet would hand out at now
// (current generation, unexpired); len(m.cache) would also count expired
// entries no sweep has reached. Unlike cacheGet it deletes nothing, so a
// /metrics scrape never changes state, and minterCacheMax keeps the walk cheap.
// The caller must hold m.mu. cachePutLocked evicts against len(m.cache), so
// cache_entries can sit below the count that drives cache_evictions.
func (m *Minter) servableCacheEntriesLocked(now time.Time) int {
	n := 0
	for _, c := range m.cache {
		if c.gen == m.gen && !now.After(c.expiry) {
			n++
		}
	}
	return n
}

// dropReportedGenerationLocked removes gen's cached tokens when gen is the
// generation a consumer reported degraded. A report-driven retirement leaves
// m.gen unchanged, so without this the cache would keep serving it; filtering
// on gen spares a newer generation's pre-mint. Every path that clears a suspect
// mark calls it, since a recycle or crash can consume a pending report first.
// An unreported generation keeps its tokens on teardown: a PO token survives
// its browser dying or an age recycle, so dropping them would force needless
// re-mints. The caller must hold m.mu.
func (m *Minter) dropReportedGenerationLocked(gen uint64) {
	if m.reportSuspectGen != gen {
		return
	}
	for k, c := range m.cache {
		if c.gen == gen {
			delete(m.cache, k)
		}
	}
}

func (m *Minter) cachePut(key string, res browser.MintResult, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cachePutLocked(key, res, gen)
}

// cachePutLocked is cachePut for a caller that already holds m.mu, such as
// ensure writing the pre-mint's entries in the same critical section that
// publishes the session.
func (m *Minter) cachePutLocked(key string, res browser.MintResult, gen uint64) {
	if gen != m.gen { // session was recycled mid-mint; don't cache a stale-gen token.
		return
	}
	// Close does not bump m.gen, so without this a request outliving the
	// shutdown drain could refill the cache Close emptied, and Tenants keeps
	// closed Minters for metrics.
	if m.closed {
		return
	}
	// A reported generation is on its way out. A mint already holding the page
	// when the report arrived still returns its token, but caching it would
	// serve the reported identity from the fast path, where no request takes the
	// page to consume the retirement.
	if m.reportSuspectGen != 0 && m.reportSuspectGen == gen {
		return
	}
	ttl := time.Duration(res.Lifetime) * time.Second
	if ttl <= 0 || ttl > minterMaxCacheTTL {
		ttl = minterMaxCacheTTL
	}
	if ttl -= minterCacheMargin; ttl < 0 {
		ttl = 0
	}
	now := time.Now() // one clock read, matching negCachePut
	expiry := now.Add(ttl)
	// Lifetime is the whole grant measured from attestation, so a late mint's
	// entry would outlive its token. ExpiresAt caps the entry, and a token with no
	// margin left is not cached.
	if !res.ExpiresAt.IsZero() {
		if capped := res.ExpiresAt.Add(-minterCacheMargin); capped.Before(expiry) {
			expiry = capped
		}
	}
	if !expiry.After(now) {
		return
	}
	// Only a new key can force eviction. Expired entries go first; if the cache is
	// still full, the live entry nearest expiry is evicted. The size invariant
	// means one eviction is enough.
	if _, exists := m.cache[key]; !exists && len(m.cache) >= minterCacheMax {
		var evictKey string
		var evictExp time.Time
		haveEvict := false
		for k, c := range m.cache {
			if now.After(c.expiry) {
				delete(m.cache, k)
				continue
			}
			if !haveEvict || c.expiry.Before(evictExp) {
				evictKey, evictExp, haveEvict = k, c.expiry, true
			}
		}
		if len(m.cache) >= minterCacheMax && haveEvict {
			delete(m.cache, evictKey)
			m.metrics.CacheEvictions.Add(1)
		}
	}
	m.cache[key] = cachedToken{res: res, expiry: expiry, gen: gen}
}

// SessionSnapshot returns an established session's identity, cookies, and
// generation. It holds mintMu so all three come from one generation, and first
// refreshes a stale or reported-degraded session so the consumer adopts a fresh
// identity. It runs the proof gate PlayerContext uses (cool-down, one relaunch
// per streak, deadline handling, unproven_rejections), so a session that cannot
// prove is refused rather than exported. No separation wait applies: the window
// keeps a served context's establishment away from a mint, and the proof itself
// is harmless (see ensureProven). The startup self-test normally proves the
// session first, so this costs nothing.
func (m *Minter) SessionSnapshot(ctx context.Context) (browser.Identity, []*http.Cookie, uint64, error) {
	m.mintMu.Lock()
	defer m.mintMu.Unlock()
	// A session lost after its proof, while its cookies are read, is replaced
	// once, like one that dies during the proof; the replacement is not itself
	// replaced.
	for replaced := false; ; replaced = true {
		sess, gen, err := m.refreshStreamingSession(ctx)
		if err != nil {
			return browser.Identity{}, nil, 0, err
		}
		// The second pass already runs on a replacement, so its proof gets none of
		// its own for a death or a bot check; otherwise one /session call could
		// serialize four launches under mintMu.
		sess, gen, err = m.prove(ctx, sess, gen, "session", !replaced)
		if err != nil {
			return browser.Identity{}, nil, 0, err
		}
		cookies, err := sess.BrowserCookies(ctx)
		if err != nil {
			// Retry once on the replacement, whatever took the session away;
			// sessionDied runs first so a browser death is logged.
			if !replaced && ctx.Err() == nil &&
				(m.sessionDied(sess, gen, "session", err) || m.isSuperseded(sess, gen)) {
				continue
			}
			return browser.Identity{}, nil, 0, err
		}
		return sess.Identity(), cookies, gen, nil
	}
}

// HealthSnapshot is a consistent view of one session generation. Its browser
// proof fields describe playback the daemon observed.
type HealthSnapshot struct {
	Identity                browser.Identity
	AttestKind              string
	Generation              uint64
	BrowserProofEstablished bool
	LastBrowserProofOutcome string
	StreamingSuspect        bool // a consumer reported this generation degraded
}

// failHealth is the return value for a non-live Health outcome. It carries the
// generation, so /ping agrees with /metrics, which reads m.gen (never reset);
// routing every failure path through it keeps a new one from reporting 0.
func failHealth(gen uint64, err error) (HealthSnapshot, bool, error) {
	return HealthSnapshot{Generation: gen}, false, err
}

// isSuperseded reports whether the live session or generation changed since
// (sess, gen) were read, so a result from sess describes a stale generation:
// Health retries instead of reporting it, and the request paths take the
// replacement (see sessionDied).
func (m *Minter) isSuperseded(sess minterSession, gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sess != sess || m.gen != gen
}

// sessionDied reports whether sess was retired out from under the request that
// holds it because the browser died, and logs the event. err then describes a
// dead page, not anything the ladders grade, and the retirement already counted
// the crash. Every teardown (retire, ensure's recycle branch, Close) records
// its cause (see retiredGen), which is read back here rather than inferred
// from which locks the retirer held: a deliberate teardown taken for a death
// would drop a real failure from the counters and hand out a free relaunch.
func (m *Minter) sessionDied(sess minterSession, gen uint64, what string, err error) bool {
	m.mu.Lock()
	superseded := m.sess != sess || m.gen != gen
	died := superseded && (m.retiredGen != gen || m.retiredCrash)
	m.mu.Unlock()
	if !died {
		return false
	}
	m.log.Info("minter: "+what+" lost its session mid-request; its failure is not graded", "gen", gen, "err", err)
	return true
}

// Health probes the existing session and returns a consistent snapshot tied to
// one generation, plus whether a live session was found. It does not call ensure,
// so it cannot launch, attest, or recycle an expired session.
//
// A failed probe is confirmed by a second before anything is retired, and a
// confirmed failure retires only if mintMu is free; otherwise Health returns
// ErrProbeBusy, so /ping neither closes a session in use nor marks a busy
// daemon unhealthy. A session replaced mid-probe is probed again, and one
// replaced on both attempts reports no-session rather than a stale failure.
// One call is thus at most four bounded round trips plus a bounded teardown;
// the server follows any answer but ok with its own browser check.
func (m *Minter) Health(ctx context.Context) (HealthSnapshot, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		m.mu.Lock()
		sess, gen := m.sess, m.gen
		m.mu.Unlock()
		if sess == nil {
			return failHealth(gen, ErrNoSession)
		}

		err := m.probe(ctx, sess)
		if err == nil {
			// A success from a session replaced mid-probe describes a stale
			// generation, so probe the current one instead.
			if m.isSuperseded(sess, gen) {
				continue
			}
			return m.healthSnapshot(sess, gen), true, nil
		}
		// Cancellation does not imply that the browser is dead.
		if ctx.Err() != nil {
			return failHealth(gen, ctx.Err())
		}
		// A failure from a replaced session says nothing about its replacement, so
		// probe that instead. Guarding on supersession alone, not attempt, lets
		// constant churn fall through to a soft no-session instead of a misleading
		// probe-failed (503) on the last attempt.
		if m.isSuperseded(sess, gen) {
			continue
		}
		// Confirm before acting. /ping can run every 30 s (the image's HEALTHCHECK
		// does on a keyless daemon), and retiring a warm generation on one
		// transient timeout would destroy a healthy session and forge a
		// browser-death signal. A fresh pingProbeTimeout window is the remedy, with
		// no delay: Session.Ping is one trivial eval that fails in microseconds on a
		// closed CDP connection, so a dead browser confirms instantly.
		cerr := m.probe(ctx, sess)
		if ctx.Err() != nil {
			return failHealth(gen, ctx.Err())
		}
		if m.isSuperseded(sess, gen) {
			continue
		}
		if cerr == nil {
			m.log.Warn("minter: ping probe recovered on confirmation; keeping the session", "gen", gen, "err", err)
			return m.healthSnapshot(sess, gen), true, nil
		}
		if !m.mintMu.TryLock() {
			// A request holds the page, so report busy rather than retire: the next
			// probe re-checks, and a browser that really died is retired by the
			// crash watcher, which needs no lock. Probe-failed would mark the
			// container unhealthy with nothing retired; busy stays HTTP 200 because
			// every mintMu holder is bounded (requestProcessTimeout for requests).
			m.metrics.ProbeBusy.Add(1)
			// Report the live session's real state with busy; failHealth's zero
			// snapshot would misreport its attest kind and proof state.
			return m.healthSnapshot(sess, gen), false, fmt.Errorf("%w: %v", ErrProbeBusy, cerr)
		}
		// Two missed round trips on a page no request holds: it is gone. Retiring
		// it as a crash counts the loss under Crashes (see ProbeFailures).
		m.metrics.ProbeFailures.Add(1)
		m.retire(gen, "ping probe failed twice: "+cerr.Error(), true)
		m.mintMu.Unlock()
		return failHealth(gen, cerr)
	}
	// Superseded on every attempt: report a soft no-session with the current
	// generation, so /ping still shows the last-known N rather than 0.
	m.mu.Lock()
	gen := m.gen
	m.mu.Unlock()
	return failHealth(gen, ErrNoSession)
}

// probe runs one bounded CDP round trip against sess. Health calls it twice
// before acting on a failure, so both probes get the same budget.
func (m *Minter) probe(ctx context.Context, sess minterSession) error {
	pctx, cancel := context.WithTimeout(ctx, pingProbeTimeout)
	defer cancel()
	return sess.Ping(pctx)
}

// healthSnapshot builds a HealthSnapshot for the probed (sess, gen). It reads the
// suspect mark under one m.mu acquisition tied to that generation so the snapshot
// never combines fields from different generations.
func (m *Minter) healthSnapshot(sess minterSession, gen uint64) HealthSnapshot {
	proof, proofAt := sess.LastProof()
	snap := HealthSnapshot{
		Identity:                sess.Identity(),
		AttestKind:              sess.AttestKind(),
		Generation:              gen,
		BrowserProofEstablished: sess.Established(),
	}
	if !proofAt.IsZero() {
		snap.LastBrowserProofOutcome = proof.Outcome
	}
	m.mu.Lock()
	snap.StreamingSuspect = m.reportSuspectGen == gen && m.reportSuspectGen != 0
	m.mu.Unlock()
	return snap
}

// SelfTest mints a GVS token for the current identity if attestation did not
// cache one, then attempts full-length establishment. It retries a failed mint
// in place without the relaunch ladder and returns a persistent failure; a
// failed proof is logged and retried by the first endpoint that needs it.
func (m *Minter) SelfTest(ctx context.Context) error {
	m.mintMu.Lock()
	defer m.mintMu.Unlock()
	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return err
	}
	vd := sess.Identity().VisitorData

	// Attestation already mints this token, so the self-test mints only when that
	// entry is missing, which means the pre-mint failed.
	if _, ok := m.cacheGet(cacheKey("gvs", vd)); !ok {
		var res browser.MintResult
		var mintErr error
		for attempt := 1; attempt <= selfTestMintAttempts; attempt++ {
			if res, mintErr = sess.Mint(ctx, vd); mintErr == nil {
				break
			}
			// Count attempts, matching the Mint path.
			m.metrics.MintFailures.Add(1)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt < selfTestMintAttempts {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(selfTestMintRetryDelay):
				}
			}
		}
		if mintErr != nil {
			return fmt.Errorf("minter: self-test mint failed after %d attempts: %w", selfTestMintAttempts, mintErr)
		}
		m.metrics.Mints.Add(1)
		m.markMinted()
		m.recordGrant(gen, res.ExpiresAt)
		// Cache under both scopes, as the pre-mint does.
		m.cachePut(cacheKey("gvs", vd), res, gen)
		m.cachePut(cacheKey("pot", vd), res, gen)
	}

	// The proof playback right after the startup mint is measured harmless, so it
	// is not held back; only a consumer's own context is. A session that already
	// proved (a rerun) is skipped: EnsureEstablished would play nothing, and
	// marking a proof would move the separation anchor for no reason.
	if !sess.Established() {
		err := sess.EnsureEstablished(ctx)
		// Any attempt, successful or not, arms the mint gate: see markPlayback.
		m.markPlayback()
		switch {
		case err == nil:
			m.markProved()
			m.log.Info("minter: self-test streaming proof passed", "gen", gen)
		case errors.Is(err, browser.ErrBotCheck):
			// No cool-down for a bot check at boot: a walled daemon would refuse its
			// first requests without ever trying a fresh identity. With no record the
			// first request meets the wall itself and relaunches at once.
			m.metrics.BotChecks.Add(1)
			m.log.Warn("minter: self-test establishment hit a bot check; the first request will relaunch for a fresh identity", "err", err)
		default:
			m.log.Warn("minter: self-test establishment failed; a later /session or /player-context request will retry", "err", err)
			// Record the failure so a request during the cool-down is refused at once
			// instead of paying for another proof. The self-test never relaunches,
			// so it leaves the streak's relaunch to ensureProven's ladder.
			m.recordProofFailure(gen, err, false)
		}
	}
	return nil
}

// lifetimeCounterKeys is the ordered set of process-lifetime counters returned
// by counterValues. Per-tenant metrics and the redacted aggregate both use this
// list, so their counter sets stay aligned.
var lifetimeCounterKeys = []string{
	"attestations",
	"mints",
	"mint_failures",
	"escalations",
	"player_contexts",
	"player_context_failures",
	"player_context_negative_cache_hits",
	"status2_rejections",
	"separation_waits",
	"unproven_rejections",
	"bot_checks",
	"unrecognized_login_refusals",
	"crashes",
	"probe_failures",
	"probe_busy",
	"cache_hits",
	"cache_misses",
	"cache_evictions",
	"launch_failures",
	"streaming_recycles",
	"report_driven_recycles",
	"degradation_reports_accepted",
	"degradation_reports_rejected_stale",
	"degradation_reports_already_retired",
	"degradation_reports_rate_limited",
	"degradation_reports_duplicate_pending",
}

// counterValues returns each process-lifetime counter keyed by its metrics name.
// Its key set must match lifetimeCounterKeys.
func (m *Minter) counterValues() map[string]int64 {
	return map[string]int64{
		"attestations":                          m.metrics.Attestations.Load(),
		"mints":                                 m.metrics.Mints.Load(),
		"mint_failures":                         m.metrics.MintFailures.Load(),
		"escalations":                           m.metrics.Escalations.Load(),
		"player_contexts":                       m.metrics.PlayerContexts.Load(),
		"player_context_failures":               m.metrics.PlayerContextFailures.Load(),
		"player_context_negative_cache_hits":    m.metrics.PlayerContextNegativeCacheHits.Load(),
		"status2_rejections":                    m.metrics.Status2Rejections.Load(),
		"separation_waits":                      m.metrics.SeparationWaits.Load(),
		"unproven_rejections":                   m.metrics.UnprovenRejections.Load(),
		"bot_checks":                            m.metrics.BotChecks.Load(),
		"unrecognized_login_refusals":           m.metrics.UnrecognizedLoginRefusals.Load(),
		"crashes":                               m.metrics.Crashes.Load(),
		"probe_failures":                        m.metrics.ProbeFailures.Load(),
		"probe_busy":                            m.metrics.ProbeBusy.Load(),
		"cache_hits":                            m.metrics.CacheHits.Load(),
		"cache_misses":                          m.metrics.CacheMisses.Load(),
		"cache_evictions":                       m.metrics.CacheEvictions.Load(),
		"launch_failures":                       m.metrics.LaunchFailures.Load(),
		"streaming_recycles":                    m.metrics.StreamingRecycles.Load(),
		"report_driven_recycles":                m.metrics.ReportDrivenRecycles.Load(),
		"degradation_reports_accepted":          m.metrics.DegradationReportsAccepted.Load(),
		"degradation_reports_rejected_stale":    m.metrics.DegradationReportsRejectedStale.Load(),
		"degradation_reports_already_retired":   m.metrics.DegradationReportsAlreadyRetired.Load(),
		"degradation_reports_rate_limited":      m.metrics.DegradationReportsRateLimited.Load(),
		"degradation_reports_duplicate_pending": m.metrics.DegradationReportsDuplicatePending.Load(),
	}
}

// MetricsSnapshot returns counters and current state for the /metrics endpoint.
//
// Session detail fields remain present after retirement so consumers can use one
// schema for live and not-live states. Fields that do not apply use sentinels:
// "" for string fields and nil map values, encoded as JSON null, for nullable
// numeric fields. streaming_seconds_until_recycle is present only when
// time-based recycling is enabled; absence means recycling is disabled.
func (m *Minter) MetricsSnapshot() map[string]any {
	m.mu.Lock()
	gen := m.gen
	sess := m.sess
	live := sess != nil
	kind := ""
	var ageSecs int
	suspect := live && m.reportSuspectGen == gen && m.reportSuspectGen != 0
	suspectVideo := ""
	if live {
		kind = sess.AttestKind()
		ageSecs = int(time.Since(m.attestedAt).Seconds())
		if suspect {
			suspectVideo = m.reportSuspectVideoID
		}
	}
	// The recycle field is controlled by static config. Its value comes from the
	// live session's armed deadline, read under m.mu. Ignore streamingDeadline
	// when no session is live because it may belong to an older generation.
	recycleEnabled := m.streamingMaxAge > 0
	var recycleSecs any // nil encodes as JSON null
	if recycleEnabled && live && !m.streamingDeadline.IsZero() {
		// Recycling waits for the next streaming handoff, so clamp an overdue
		// deadline to zero rather than reporting a negative remaining time.
		if secs := int(time.Until(m.streamingDeadline).Seconds()); secs > 0 {
			recycleSecs = secs
		} else {
			recycleSecs = 0
		}
	}
	cacheN := m.servableCacheEntriesLocked(time.Now())
	m.mu.Unlock()

	// Read the session's proof state outside m.mu, as healthSnapshot does. The
	// session guards these fields with probeMu, and Close does not mutate them.
	var established bool
	var proofOutcome string
	var proofAge any // nil encodes as JSON null; int means an outcome time is known
	if live {
		established = sess.Established()
		if proof, proofAt := sess.LastProof(); !proofAt.IsZero() {
			proofOutcome, proofAge = proof.Outcome, int(time.Since(proofAt).Seconds())
		}
	}

	out := map[string]any{
		"generation":       gen,
		"session_live":     live,
		"attest_kind":      kind,
		"session_age_secs": ageSecs,
		"cache_entries":    cacheN,
		// Keep these detail fields present in every state (see the doc comment).
		"browser_proof_established":   established,
		"streaming_suspect":           suspect,
		"streaming_suspect_video":     suspectVideo,
		"last_browser_proof_outcome":  proofOutcome,
		"last_browser_proof_age_secs": proofAge,
	}
	for name, v := range m.counterValues() {
		out[name] = v
	}
	// Emit only when time-based recycling is enabled. A null value means recycling
	// is enabled but no live session has an armed deadline.
	if recycleEnabled {
		out["streaming_seconds_until_recycle"] = recycleSecs
	}
	return out
}

// Close tears down the live session and makes the Minter terminal: ensure will
// not launch again, so a request still in flight cannot walk its relaunch ladder
// into a browser nobody will close. It does not take mintMu, so it never waits
// on an in-flight browser call.
func (m *Minter) Close() {
	m.mu.Lock()
	m.closed = true
	s := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.reportSuspectGen = 0
	m.reportSuspectVideoID = ""
	m.retiredGen, m.retiredCrash = m.gen, false
	// Replace the maps rather than clear them so their storage is reclaimed:
	// Tenants keeps closed Minters for metrics.
	m.cache = make(map[string]cachedToken)
	m.negCache = make(map[string]negEntry)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s != nil {
		s.Close()
	}
}
