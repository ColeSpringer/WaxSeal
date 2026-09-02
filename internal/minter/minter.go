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

	mu         sync.Mutex
	sess       minterSession
	gen        uint64 // bumps on each (re)attest; invalidates older cache entries
	attestedAt time.Time
	// grantExpiresAt is when the tokens of the current generation expire. Every
	// token an attestation mints carries the same expiry, so one mint reveals it
	// for the whole generation. Zero means no mint has reported one yet, in which
	// case maxAge alone bounds the session.
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
	// completed an in-page mint, a full-length proof playback, and a context
	// establishment. waitSeparation keeps mintSeparation between the served
	// context and the later of the mint and the proof, and between a mint and the
	// last establishment. A context handed out earlier does not extend the
	// window: consecutive handoffs are graded fine. All three are cleared when a
	// new generation is published, because they describe one page's history.
	lastMintAt      time.Time
	lastProofAt     time.Time
	lastEstablishAt time.Time

	// proofFailGen and proofFailedAt record the generation and time of that
	// generation's most recent failed full-length proof, so ensureProven can
	// refuse a repeat request on cool-down instead of paying another proof
	// attempt. Both are cleared when a new generation is published and whenever a
	// proof succeeds.
	proofFailGen  uint64
	proofFailedAt time.Time
	// proofCooldownWarnedAt is the proofFailedAt value the cool-down warning has
	// already fired for, so repeated refusals within the same cool-down window log
	// the warning once instead of once per refused request. Cleared alongside
	// proofFailGen and proofFailedAt.
	proofCooldownWarnedAt time.Time

	// proofRelaunched marks that the current failure streak already spent its one
	// proof-driven relaunch. The relaunch backoff elsewhere in the pool only
	// rate-limits how often a relaunch can happen; it does not stop a
	// persistently unprovable environment from restarting Chromium again on
	// every cool-down forever. This flag is that stop: it survives a new
	// generation, however that generation came to exist, and only a successful
	// proof (markProved) clears it.
	proofRelaunched bool

	// retiredGen and retiredCrash record the last generation torn down and
	// whether the browser died under it. sessionDied reads them back instead of
	// inferring the cause from which locks a retirer needed, so a recycle that
	// happens off the request path cannot be misread as a browser death. Only the
	// newest teardown is kept: retirement runs in generation order, so a request
	// whose generation is older than retiredGen has been parked across a whole
	// launch cycle and has no record of its own left, and is treated as a death
	// (which is what the lock-ordering inference did for every supersession).
	retiredGen   uint64
	retiredCrash bool

	// shortGrantWarnedGen is the generation the unusable-grant warning has already
	// fired for, so a generation whose every mint reports the same expiry inside
	// the cache margin logs it once rather than once per mint.
	shortGrantWarnedGen uint64

	mintMu  sync.Mutex // serializes the in-browser mint calls (single page)
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
// attempts, not requests. PlayerContextFailures also counts negative-cache hits.
type minterMetrics struct {
	Attestations   atomic.Int64
	LaunchFailures atomic.Int64
	Mints          atomic.Int64
	MintFailures   atomic.Int64 // per attempt (see minterMetrics doc)
	// Escalations counts the decision to abandon a session generation after
	// repeated failures on the mint, player-context, or proof ladder, whether the
	// relaunch of a fresh generation happens immediately or is deferred to the
	// next request that touches this Minter.
	Escalations    atomic.Int64
	CacheHits      atomic.Int64
	CacheMisses    atomic.Int64
	CacheEvictions atomic.Int64 // positive-cache entries evicted at capacity
	// Crashes counts unexpected browser loss detected by CDP or a health probe.
	// Intentional session retirement does not count.
	Crashes               atomic.Int64
	PlayerContexts        atomic.Int64
	PlayerContextFailures atomic.Int64 // failed attempts and negative-cache hits
	// Status2Rejections counts refused requests where a player context could not
	// be confirmed beyond the status-2 cap. Attempts are counted by
	// PlayerContextFailures; this counter is once per request.
	Status2Rejections atomic.Int64
	// SeparationWaits counts operations held back to keep an in-page mint and a
	// context establishment mintSeparation apart. One wait can cover one request.
	SeparationWaits atomic.Int64
	// UnprovenRejections counts player-context requests refused because no
	// session proved full-length streaming for them: a proof that failed, a
	// cool-down from an earlier failure, or a browser that died mid-proof and
	// left the request nothing to serve from. The death is not graded against the
	// next generation, but the refusal is still the request's outcome and is
	// counted here, since no other counter records it. Once per request.
	UnprovenRejections atomic.Int64

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
	// whose retirement is already queued for the next streaming handoff. The
	// retirement itself is only counted once, by the report that queued it.
	DegradationReportsDuplicatePending atomic.Int64
}

const (
	minterMaxCacheTTL   = 6 * time.Hour
	minterCacheMargin   = 5 * time.Minute // don't hand out a token within this of expiry
	minterDefaultMaxAge = 11 * time.Hour  // < the ~12h integrity lifetime
	minterNegCacheTTL   = 5 * time.Minute // remember an unplayable video_id this long
	minterNegCacheMax   = 256             // bound the negative cache
	// minterCacheMax bounds the positive token cache. Positive entries are keyed by
	// distinct video or visitor identities, so this cache is intentionally larger
	// than the negative cache. The bound keeps per-tenant memory predictable, about
	// 0.8 MB for typical entries and about 10 MB for unusually large tokens.
	minterCacheMax = 1024

	// DefaultReportDebounce limits report-driven re-attestation to a sustained
	// 12 times per hour. Bursts up to ReportBurst are allowed first, so the
	// debounce is the refill interval of the report budget rather than a hard
	// spacing between recycles.
	DefaultReportDebounce = 5 * time.Minute

	// ReportBurst is how many report-driven recycles may happen back to back
	// before rate-limiting. It is sized for the largest rotation sequence a
	// well-behaved consumer performs in one operation: a bulk-enumeration
	// throttle escape retires its identity and re-asks up to four times before
	// giving up, and a mid-sequence decline makes the consumer misread
	// throttled entries as gone. A full budget covers that whole sequence; a
	// budget drained by recent reports may not, since it refills at only one
	// token per debounce interval, which is what keeps the sustained rate at
	// one recycle per interval.
	ReportBurst = 4

	// defaultMintSeparation is how far apart the daemon keeps an in-page mint and
	// a context establishment. A consumer that streamed a context established
	// 0.6 s from the mint of the token it sent was graded a preview every time
	// (0 of 6 streams ran to completion), while the same measurement at 10.6 s
	// ran full length every time (6 of 6). 12 s is that measured edge plus
	// margin. WAXSEAL_MINT_SEPARATION overrides it.
	defaultMintSeparation = 12 * time.Second

	// mintSeparationEnv overrides defaultMintSeparation with a positive Go
	// duration, for example "20s".
	mintSeparationEnv = "WAXSEAL_MINT_SEPARATION"

	// mintSeparationWarn marks an override large enough that a first context has
	// to wait most of the way to the per-request budget before it is served.
	mintSeparationWarn = 60 * time.Second

	// proofRetryCooldown bounds how often a session that failed to prove
	// full-length streaming pays another proof attempt. A proof can run up to a
	// minute under mintMu, so retrying it on every request would make a
	// permanently broken session hold up every other request behind it and still
	// refuse each one; the cool-down lets most of them fail fast instead.
	proofRetryCooldown = 30 * time.Second

	// pingProbeTimeout allows for a busy host without leaving /ping unbounded.
	pingProbeTimeout = 5 * time.Second

	// A short retry window tolerates transient startup failures without hiding a
	// persistent minting failure.
	selfTestMintAttempts = 3
)

// selfTestMintRetryDelay is variable so tests can shorten the retry interval.
var selfTestMintRetryDelay = 1 * time.Second

// ErrNoSession reports that the tenant has no existing attested session. It is
// exported so callers (e.g. server /ping) can distinguish the benign no-session
// state from a real probe failure.
var ErrNoSession = errors.New("waxseal: no attested session")

// ErrUnproven reports that the browser session could not prove full-length
// streaming, so no context was handed out. A context that would be the session's
// first playback is graded a preview about as often as not, so the daemon refuses
// rather than serving one. It is exported so a caller can tell a refusal apart
// from an extraction failure.
var ErrUnproven = errors.New("waxseal: session has not proved full-length streaming")

// NewMinter builds a single-identity minter for video (the landing watch id). It
// launches a browser only when an operation first needs a session.
// streamingMaxAge forces a fresh session on the next streaming handoff once the
// current one exceeds that age (0 disables); reportDebounce is the refill
// interval of the report-driven recycle budget, which allows bursts up to
// ReportBurst (<=0 uses DefaultReportDebounce). mintSeparation, when positive,
// overrides the env-derived spacing kept between an in-page mint and a context
// establishment (see resolveMintSeparation); a non-positive value keeps that
// env-derived default.
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

// mintSeparationUnparseableOnce and mintSeparationLargeOnce each log their
// warning at most once per process. NewMinter runs mintSeparationFromEnv once per
// tenant constructor, and a fleet of tenants sharing one WaxSeal process also
// shares one WAXSEAL_MINT_SEPARATION, so without this every tenant would repeat
// the identical warning at startup.
var (
	mintSeparationUnparseableOnce sync.Once
	mintSeparationLargeOnce       sync.Once
)

// mintSeparationFromEnv reads the mint-to-establishment spacing. The environment
// value wins when it parses as a positive Go duration; anything else keeps the
// default and logs why, once per process.
func mintSeparationFromEnv(log *slog.Logger) time.Duration {
	raw := os.Getenv(mintSeparationEnv)
	if raw == "" {
		return defaultMintSeparation
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		mintSeparationUnparseableOnce.Do(func() {
			log.Warn("minter: ignoring "+mintSeparationEnv+"; want a positive Go duration such as 20s",
				"value", raw, "using", defaultMintSeparation)
		})
		return defaultMintSeparation
	}
	if d > mintSeparationWarn {
		mintSeparationLargeOnce.Do(func() {
			log.Warn("minter: "+mintSeparationEnv+" is large; values near the request budget make first contexts time out",
				"value", d)
		})
	}
	return d
}

// resolveMintSeparation returns mintSeparation when it is positive, an explicit
// constructor override such as server.Config.MintSeparation. A non-positive
// value (the common case: no override was configured) falls back to the
// env-derived default, so WAXSEAL_MINT_SEPARATION keeps working for callers that
// never set the constructor option.
func resolveMintSeparation(mintSeparation time.Duration, log *slog.Logger) time.Duration {
	if mintSeparation > 0 {
		return mintSeparation
	}
	return mintSeparationFromEnv(log)
}

// waitSeparation blocks until mintSeparation has passed since from, so in-page
// attestation work and a context establishment never land within that window of
// each other in either order. what names the operation being held back and after
// names the anchor the window is measured from, for the log line. A zero from
// (nothing recorded on this session yet) and a non-positive separation both
// return immediately.
//
// Every caller reaches this while holding mintMu and keeps holding it for the
// whole wait: releasing it here would let other page work slip into the window
// and reset the anchor being measured against. The cost is bounded by
// mintSeparation and normally paid once per generation, and it can hold up
// whatever else is waiting on mintMu behind it by at most that long, such as a
// /ping probe's retirement of a dead session or a cache-missing /get_pot.
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
// in-page playback attempt (see markPlayback), successful or not: a failed
// establishment still touched the page, so a mint served right after it is
// graded the same as one served right after a successful context.
func (m *Minter) waitBeforeMint(ctx context.Context) error {
	m.mu.Lock()
	establishAt := m.lastEstablishAt
	m.mu.Unlock()
	return m.waitSeparation(ctx, establishAt, "mint", "establishment")
}

// waitBeforeEstablish holds a context establishment back until it is
// mintSeparation clear of the last in-page attestation work on this session,
// which is the later of the token mint and the proof playback. A context handed
// out earlier is not an anchor: a second context taken moments after the first
// streams full length. what names the caller.
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
// a context. A context that is the session's first playback is graded a preview
// about as often as not, so the proof runs first and the request is refused when
// it cannot pass. The proof happens once per session (the startup self-test
// normally performs it before any request arrives), and it is not held back by
// the separation window: the daemon's own playback right after a mint is
// harmless, only the context it serves a consumer is not.
//
// A session that cannot prove is not retried on every request: the first
// failure on a generation starts proofRetryCooldown, during which later
// requests are refused at once without another proof attempt. A second failure
// once the cool-down has passed is treated like the mint and player-context
// ladders' second level: the session is relaunched once and the fresh session is
// proved in its place, so a permanently broken session does not refuse every
// request forever by itself. That relaunch is spent once per failure streak
// rather than once per generation; see proofRelaunched.
//
// A proof attempt that ends because the caller's own context ended is handled
// two ways. A plain cancellation means the caller left, so nothing is recorded.
// A proof that instead ran out the request's own deadline means the environment
// failed to prove within the budget the handler gave it, not that the caller
// left, so it is recorded like any other failure: recording it lets the
// cool-down bound the next request instead of letting it burn the whole budget
// again under mintMu. Either way the returned error is ctx.Err(), so the
// server's timeout mapping is unchanged.
//
// A browser that dies while the proof is in flight is the one retirement that
// can happen under mintMu (see sessionDied). That is not a proof failure: the
// dead page says nothing about whether the session could stream, so it is
// neither recorded against the cool-down nor allowed to spend the streak's
// relaunch. The request takes the replacement ensure publishes and proves that
// instead, once; a replacement that dies too is refused, and the next request
// relaunches.
//
// ensureProven returns the session and generation the caller should use for the
// rest of the request: sess and gen unchanged on success or a refusal, or the
// replacement pair after a second failure that still had its relaunch to spend
// or after the browser died mid-proof. what names the caller ("player-context"
// or "session") and appears on every warn line this function emits, so a log
// reader can tell which endpoint paid for a given proof attempt or refusal.
func (m *Minter) ensureProven(ctx context.Context, sess minterSession, gen uint64, what string) (minterSession, uint64, error) {
	return m.prove(ctx, sess, gen, what, true)
}

// prove is ensureProven's body. replaceOnDeath allows one replacement when the
// browser dies during the proof; the replacement is proved with it false, so a
// request takes at most one replacement here.
func (m *Minter) prove(ctx context.Context, sess minterSession, gen uint64, what string, replaceOnDeath bool) (minterSession, uint64, error) {
	if sess.Established() {
		return sess, gen, nil
	}

	m.mu.Lock()
	onCooldown := m.proofFailGen == gen && time.Since(m.proofFailedAt) < proofRetryCooldown
	var remaining time.Duration
	var warnCooldown bool
	if onCooldown {
		remaining = (proofRetryCooldown - time.Since(m.proofFailedAt)).Round(time.Millisecond)
		// Warn once per cool-down window: repeated refusals compare the window's
		// own proofFailedAt against the last value warned about, rather than firing
		// on every refused request.
		if m.proofCooldownWarnedAt != m.proofFailedAt {
			m.proofCooldownWarnedAt = m.proofFailedAt
			warnCooldown = true
		}
	}
	m.mu.Unlock()
	if onCooldown {
		if warnCooldown {
			m.log.Warn("minter: refusing "+what+"; session is in a proof cool-down",
				"what", what, "gen", gen, "remaining", remaining)
		}
		m.log.Debug("minter: refusing "+what+"; session is in a proof cool-down",
			"what", what, "gen", gen, "remaining", remaining)
		m.metrics.UnprovenRejections.Add(1)
		return sess, gen, ErrUnproven
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
		// A proof that ran out the deadline has no budget left to prove a
		// replacement, so the request is refused; nothing is recorded, since the
		// dead generation is not the one the next request will find.
		if ctx.Err() != nil {
			m.metrics.UnprovenRejections.Add(1)
			return sess, gen, ctx.Err()
		}
		if replaceOnDeath {
			newSess, newGen, err := m.ensure(ctx)
			if err != nil {
				return nil, 0, err
			}
			return m.prove(ctx, newSess, newGen, what, false)
		}
		m.log.Warn("minter: refusing "+what+"; the replacement session died during its proof as well",
			"gen", gen, "err", proofErr)
		m.metrics.UnprovenRejections.Add(1)
		return sess, gen, fmt.Errorf("%w: %w", ErrUnproven, proofErr)
	}
	deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
	m.log.Warn("minter: refusing "+what+"; session could not prove full-length streaming",
		"gen", gen, "err", proofErr)

	secondFailure, relaunch := m.recordProofFailure(gen, true)
	if !secondFailure {
		// This refusal is returned to the caller either way (as ctx.Err() or as a
		// wrapped ErrUnproven), so it counts once here rather than once per branch.
		m.metrics.UnprovenRejections.Add(1)
		if deadline {
			return sess, gen, ctx.Err()
		}
		return sess, gen, fmt.Errorf("%w: %w", ErrUnproven, proofErr)
	}
	if !relaunch {
		m.log.Warn("minter: proof-driven relaunch for this failure streak was already spent; the session will be recycled by a successful proof, a crash, a consumer report, or the max-age recycle",
			"what", what, "gen", gen)
		m.metrics.UnprovenRejections.Add(1)
		if deadline {
			return sess, gen, ctx.Err()
		}
		return sess, gen, fmt.Errorf("%w: %w", ErrUnproven, proofErr)
	}

	// A second failure on the same generation, past the cool-down, with this
	// failure streak's relaunch still unspent (recordProofFailure just claimed
	// it): relaunch once and prove the fresh session.
	m.metrics.Escalations.Add(1)
	if deadline {
		// ctx's own deadline is what ended this proof, so this request has no
		// budget left to launch and prove a fresh session. Retire the generation
		// and let the next request's ensure relaunch instead; do not launch here.
		// This request is refused, so it is counted like the branches above.
		m.metrics.UnprovenRejections.Add(1)
		m.retire(gen, "proof timed out twice; relaunching on the next request", false)
		return sess, gen, ctx.Err()
	}
	m.retire(gen, "proof failed twice; relaunching", false)
	newSess, newGen, err := m.ensure(ctx)
	if err != nil {
		// A failed launch is already counted by launch_failures inside ensure, so unproven_rejections is deliberately not incremented here.
		return nil, 0, err
	}
	proofErr = newSess.EnsureEstablished(ctx)
	m.markPlayback()
	if proofErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return newSess, newGen, ctx.Err()
		}
		newDeadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
		// A browser that died under the fresh generation is refused with the same
		// wording the entry path uses, and nothing is held against the next
		// generation: a dead page's proof says nothing about whether the session
		// could stream. Checking it first keeps the "could not prove full-length
		// streaming" warn below off a crash, which sessionDied's own line would
		// otherwise contradict on the next line of the log.
		if m.sessionDied(newSess, newGen, what+" proof", proofErr) {
			m.metrics.UnprovenRejections.Add(1)
			if newDeadline {
				return newSess, newGen, ctx.Err()
			}
			m.log.Warn("minter: refusing "+what+"; the replacement session died during its proof as well",
				"gen", newGen, "err", proofErr)
			return newSess, newGen, fmt.Errorf("%w: %w", ErrUnproven, proofErr)
		}
		m.log.Warn("minter: refusing "+what+"; relaunched session could not prove full-length streaming",
			"gen", newGen, "err", proofErr)
		// Record the fresh generation's own failure, but never let this call claim
		// the failure streak's relaunch: that grant was already spent (or kept
		// unspent) by the recordProofFailure call above, and a generation that
		// ensure just published cannot legitimately carry a prior failure of its
		// own. secondFailure should therefore always be false here; a true value
		// would mean gen and newGen collided, which is worth a warn rather than a
		// silent second relaunch.
		if secondFailure, _ := m.recordProofFailure(newGen, false); secondFailure {
			m.log.Warn("minter: relaunched session's generation already carried a proof-failure record; a freshly published generation should never carry one",
				"what", what, "gen", newGen)
		}
		// The whole call is refused here: the first failure on gen only triggered
		// the relaunch and was never itself returned to a caller, so this is the
		// request's only rejection.
		m.metrics.UnprovenRejections.Add(1)
		if newDeadline {
			return newSess, newGen, ctx.Err()
		}
		return newSess, newGen, fmt.Errorf("%w: %w", ErrUnproven, proofErr)
	}
	m.markProved()
	return newSess, newGen, nil
}

// recordProofFailure marks generation gen's most recent proof attempt as
// failed and reports whether this is gen's second recorded failure and, if so,
// whether this failure streak still has its one relaunch to spend. claimRelaunch
// tells recordProofFailure whether this call is even eligible to spend the
// streak's relaunch: a caller passes false when it must record a failure but must
// never itself trigger a relaunch, such as the self-test (which never relaunches)
// or a check performed on a generation that was just published (which cannot
// legitimately own the streak's grant). A streak's relaunch is granted at most
// once: a caller that receives relaunchGranted == true is the sole owner of that
// grant, because claiming it (setting proofRelaunched) happens in the same
// critical section that decides it is available. gen is always the caller's
// current generation, so mintMu (held by every caller through Mint or
// PlayerContext) rules out a concurrent claim on a different generation racing
// this one.
func (m *Minter) recordProofFailure(gen uint64, claimRelaunch bool) (secondFailure, relaunchGranted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	secondFailure = m.proofFailGen == gen
	m.proofFailGen = gen
	m.proofFailedAt = time.Now()
	if secondFailure && claimRelaunch && !m.proofRelaunched {
		m.proofRelaunched = true
		relaunchGranted = true
	}
	return secondFailure, relaunchGranted
}

// markMinted records a completed in-page mint.
func (m *Minter) markMinted() {
	m.mu.Lock()
	m.lastMintAt = time.Now()
	m.mu.Unlock()
}

// markEstablished records a completed context establishment.
func (m *Minter) markEstablished() {
	m.mu.Lock()
	m.lastEstablishAt = time.Now()
	m.mu.Unlock()
}

// markPlayback records that the page attempted in-page playback establishment,
// successfully or not: sess.PlayerContext in PlayerContext and
// sess.EnsureEstablished in ensureProven and SelfTest each call it after every
// attempt. A failed establishment still touched the page, so it arms the mint
// gate (waitBeforeMint) exactly like a successful one; markEstablished and
// markProved additionally run on the success paths for their own bookkeeping
// (the context-gate anchor lastProofAt, and clearing the proof-failure state).
func (m *Minter) markPlayback() {
	m.mu.Lock()
	m.lastEstablishAt = time.Now()
	m.mu.Unlock()
}

// markProved records a completed full-length proof playback. A proof is also an
// establishment as far as the mint gate is concerned, so it sets both marks, and
// it clears any pending proof-failure cool-down: the session just demonstrated it
// can establish, so a stale failure record for it is moot.
func (m *Minter) markProved() {
	m.mu.Lock()
	now := time.Now()
	m.lastProofAt = now
	m.lastEstablishAt = now
	m.proofFailGen = 0
	m.proofFailedAt = time.Time{}
	m.proofCooldownWarnedAt = time.Time{}
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
// holds mintMu like every other caller of ensure: ensure's recycle branch closes
// an aged session, and a request already holding that session would otherwise
// find it torn down by a caller it could not serialize against. Production runs
// Warm before Serve, but the lock makes that ordering a convenience rather than
// a correctness condition.
func (m *Minter) Warm(ctx context.Context) error {
	m.mintMu.Lock()
	defer m.mintMu.Unlock()
	_, _, err := m.ensure(ctx)
	return err
}

// grantExhaustedLocked reports whether the generation's tokens are inside the
// cache margin of expiring. Past that point every token this session can mint is
// one the daemon would refuse to cache, so the session has nothing left to give
// however young its attestation is. A zero grant (no mint has reported an expiry)
// exhausts nothing. The caller must hold m.mu.
//
// It is the one piece the two recycle predicates share: the max-age half of each
// stays its own, because they answer different questions.
func (m *Minter) grantExhaustedLocked(now time.Time) bool {
	return !m.grantExpiresAt.IsZero() && !now.Before(m.grantExpiresAt.Add(-minterCacheMargin))
}

// grantWorthRecording reports whether an attestation issued tokens with enough
// life to be worth recycling for. It measures the grant from the attestation
// rather than from now, which is what separates the two ways a grant can be
// spent:
//
// A grant that runs down over time is the case the recycle exists for. A fresh
// attestation gets a fresh grant, so tearing the session down converges.
//
// A grant that arrives already inside the cache margin does not. Its replacement
// arrives just as exhausted, so recycleCauseLocked would fire on the session it
// just published and every request would tear down a healthy session under
// mintMu, forever, with no metric moving and no cache ever warming
// (cachePutLocked declines those tokens either way). Such a grant is dropped: its
// tokens stay uncacheable, which is the honest outcome, and maxAge alone bounds
// the session.
func grantWorthRecording(expiresAt, attestedAt time.Time) bool {
	return !expiresAt.IsZero() && expiresAt.After(attestedAt.Add(minterCacheMargin))
}

// recordGrantLocked latches gen's token expiry, reporting whether it was dropped
// for arriving inside the cache margin so the caller can warn after unlocking. It
// warns once per generation. attestedAt is when gen was attested, which is what
// the grant is measured against. The caller must hold m.mu.
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

// warnShortGrant reports an attestation whose tokens were already inside the
// cache margin when it issued them. The session keeps serving those tokens
// uncached until maxAge recycles it, because bounding it by this grant would
// relaunch on every request without ever reaching a session that could serve one.
func (m *Minter) warnShortGrant(gen uint64, expiresAt time.Time) {
	m.log.Warn("minter: attestation granted tokens that were already inside the cache margin; not bounding the session by them",
		"gen", gen, "expires_in", time.Until(expiresAt).Round(time.Second), "margin", minterCacheMargin)
}

// recordGrant remembers generation gen's token expiry, which bounds the session
// alongside maxAge. Every token an attestation mints carries the same expiry, so
// the first mint to report one fixes it and later mints repeat the same value. A
// zero expiry records nothing, and a mint that completed against a generation the
// session has since left records nothing either, matching cachePut.
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
// path checks it because the recycle, and the cache clear that follows a fresh
// attestation, live inside ensure.
//
// This keys on attestedAt rather than on a live session. During the relaunch
// window m.sess is nil while m.gen has not yet bumped, so the cache still holds
// servable entries from the aged generation; keying on m.sess != nil would report
// fresh there and hand one out. A zero attestedAt (never attested) is not past
// any bound.
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
		// This path bypasses retire, so it must also clear the suspect mark.
		if cause := m.recycleCauseLocked(); cause != "" {
			old, gen, age := m.sess, m.gen, time.Since(m.attestedAt)
			m.sess = nil
			cancel := m.watchCancel
			m.watchCancel = nil
			m.reportSuspectGen = 0
			m.reportSuspectVideoID = ""
			// A recycle is not a crash. Recording it keeps a request that was
			// holding this generation from reading its own failure as a browser
			// death (see sessionDied).
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

		// Mint the visitor's GVS token before the session is published. Every other
		// caller is still parked on the single-flight channel, so this mint cannot
		// interleave with another call on the same page, and it puts the token a
		// consumer streams with well ahead of the first context establishment,
		// which is the spacing waitSeparation then maintains. A failure here is not
		// fatal: the session is published anyway and the next token request mints
		// on demand.
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
			return nil, 0, err
		}
		m.sess = sess
		m.gen++
		// Fixed before the pre-mint's grant is recorded, which is measured against it.
		m.attestedAt = time.Now()
		// A new page has no mint, proof, or establishment behind it, and no prior
		// generation's proof failure applies to it. The pre-mint above, when it
		// succeeded, is this generation's first mark.
		m.lastMintAt = time.Time{}
		m.lastProofAt = time.Time{}
		m.lastEstablishAt = time.Time{}
		m.grantExpiresAt = time.Time{}
		m.proofFailGen = 0
		m.proofFailedAt = time.Time{}
		m.proofCooldownWarnedAt = time.Time{}
		// A generation bump invalidates every cached token. Under m.mu, no
		// new-generation cachePut can interleave before the clear, so every existing
		// entry belongs to the old session. Clear only the positive cache: an
		// unplayable video remains unplayable across a relaunch.
		clear(m.cache)
		if preminted {
			m.lastMintAt = time.Now()
			// The pre-mint reports this generation's token expiry, which bounds the
			// session alongside maxAge. A result without one leaves it unbounded here
			// and a later mint may still report it, and one that is already inside
			// the cache margin is dropped rather than bounding the session to a
			// length it cannot serve.
			shortGrant = m.recordGrantLocked(m.gen, m.attestedAt, premint.ExpiresAt)
			// Cache the pre-mint under both the gvs and the default (pot) scope: scope
			// only namespaces the cache and the token is identical either way, so a
			// consumer that asks for either gets the hit. Writing it here, in the same
			// critical section that publishes m.sess, means a waiter released by
			// close(ch) above can never observe the session without also seeing its
			// token, even though that waiter cannot actually proceed until this
			// critical section ends.
			m.cachePutLocked(cacheKey("gvs", premintBinding), premint, m.gen)
			m.cachePutLocked(cacheKey("pot", premintBinding), premint, m.gen)
		}
		// Arm the streaming deadline for this generation.
		if m.streamingMaxAge > 0 {
			m.streamingDeadline = time.Now().Add(jitter(m.streamingMaxAge))
		}
		g := m.gen
		// The crash watcher must outlive the (transient) launch ctx, so give it a
		// session-scoped context cancelled only when this session is torn down.
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
// loses the CDP connection. That lets the next request relaunch instead of first
// failing against a dead session, and a request that already holds the session
// when it dies takes the replacement rather than grading the dead page (see
// sessionDied). Only browser.Session exposes the event stream; test fakes are
// ignored.
//
// The context is tied to the session lifetime, not the launch request. WaitCrash
// returns a reason on browser loss and "" when ctx is cancelled.
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

// refreshStreamingSession replaces a stale or reported-degraded session before a
// streaming handoff. The caller must hold mintMu. Token-only requests bypass this
// check so they do not recycle an otherwise usable session.
func (m *Minter) refreshStreamingSession(ctx context.Context) (minterSession, uint64, error) {
	m.mu.Lock()
	cur := m.gen
	live := m.sess != nil
	suspect := live && m.reportSuspectGen == cur && cur != 0
	stale := live && !m.streamingDeadline.IsZero() && time.Now().After(m.streamingDeadline)
	m.mu.Unlock()

	if live && (suspect || stale) {
		// retire verifies that cur is still current.
		reason := "streaming session exceeded max age; relaunching"
		if suspect {
			reason = "consumer reported degradation; relaunching"
		}
		if m.retire(cur, reason, false) {
			if suspect {
				m.metrics.ReportDrivenRecycles.Add(1)
				// Deferred and immediate report-driven recycles share one budget.
				m.mu.Lock()
				m.spendReportTokenLocked()
				m.mu.Unlock()
			} else {
				m.metrics.StreamingRecycles.Add(1)
			}
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
// indicates that the report applies to the current session. Retired indicates
// that the session was closed immediately. RetirementPending indicates that it
// will be closed at the next streaming handoff. RetryAfterSeconds is set when the
// report was rate-limited.
type ReportResult struct {
	Accepted          bool
	Retired           bool
	RetirementPending bool
	Generation        uint64
	RetryAfterSeconds int
}

// ReportDegraded records that generation gen produced a degraded stream. videoID
// and reason are diagnostic. The report is rate-limited and applies only to the
// current generation. If a browser operation is in progress, retirement is
// deferred until the next streaming handoff.
func (m *Minter) ReportDegraded(gen uint64, videoID, reason string) ReportResult {
	// Marking the generation before releasing m.mu deduplicates concurrent reports.
	m.mu.Lock()
	cur := m.gen
	m.refillReportTokensLocked(time.Now())
	switch {
	case gen != cur:
		// A genuinely old or future generation: the reported session was already
		// replaced (or never existed). A no-op.
		m.mu.Unlock()
		m.metrics.DegradationReportsRejectedStale.Add(1)
		return ReportResult{Accepted: false, Generation: cur}
	case m.sess == nil:
		// The current generation, but its session was already retired (a crash or a
		// prior report) in the brief window before the next request relaunches. A
		// benign no-op distinct from a stale report. This case is load-bearing in
		// its position: a report-driven retire leaves gen unchanged and spends
		// report budget, so this predicate and the rate-limit predicate
		// (reportTokens < 1) can both be true for a re-report of the same gen. It
		// must precede the pending and rate-limit cases, or an already-retired
		// report is miscounted as rate-limited. TestMinterReportDegradedAlreadyRetired
		// pins the ordering (already_retired == 1, rate_limited == 0).
		m.mu.Unlock()
		m.metrics.DegradationReportsAlreadyRetired.Add(1)
		return ReportResult{Accepted: false, Generation: cur}
	case m.reportSuspectGen == gen:
		// Retirement is already queued for the next streaming handoff.
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
	m.metrics.DegradationReportsAccepted.Add(1)
	m.mu.Unlock()

	// Only the first report for this generation attempts immediate retirement.
	if m.mintMu.TryLock() {
		acted := m.retire(gen, "consumer report: "+reason, false)
		// Spend report budget only when this report actually recycles the session.
		// If a crash watcher or max-age retirement already closed the generation,
		// retire is a no-op and should not eat into the next real report's budget.
		if acted {
			m.mu.Lock()
			m.spendReportTokenLocked()
			m.mu.Unlock()
			m.metrics.ReportDrivenRecycles.Add(1)
		}
		m.mintMu.Unlock()
		return ReportResult{Accepted: true, Retired: acted, Generation: gen}
	}
	// A browser operation holds mintMu; defer retirement to the next handoff.
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

// spendReportTokenLocked consumes one token of report budget. Called only after
// a report-driven retire actually recycled the session, so a no-op retire never
// spends budget. The caller must hold m.mu.
func (m *Minter) spendReportTokenLocked() {
	m.refillReportTokensLocked(time.Now())
	m.reportTokens = max(m.reportTokens-1, 0)
}

// Mint returns a token for (scope, binding), reporting whether it came from cache.
// The retry policy serves cached tokens first, retries one failed mint in place,
// then relaunches and attests before the final attempt. Repeated requests for the
// same binding continue to use the cached token.
func (m *Minter) Mint(ctx context.Context, scope, binding string) (res browser.MintResult, cached bool, err error) {
	key := cacheKey(scope, binding)
	// A cache-hit-only workload never reaches ensure, where the recycle lives, so
	// an aged session would serve its cached tokens indefinitely. Skipping the
	// lookup is what forces this request through ensure instead.
	if !m.sessionPastMaxAge() {
		if r, ok := m.cacheGet(key); ok {
			m.metrics.CacheHits.Add(1)
			return r, true, nil
		}
	}

	m.mintMu.Lock() // one page, so mints serialize
	defer m.mintMu.Unlock()
	// Another goroutine may have filled the cache while this call waited for
	// mintMu, and the session may have crossed its bound during that wait. The age
	// is read again rather than carried across the wait: a request parked behind a
	// full-length proof would otherwise hand out the generation the first read
	// found fresh, which is the one this check exists to stop serving.
	if !m.sessionPastMaxAge() {
		if r, ok := m.cacheGet(key); ok {
			m.metrics.CacheHits.Add(1)
			return r, true, nil
		}
	}

	sess, gen, err := m.ensure(ctx)
	if err != nil {
		// A lookup skipped above expected ensure to replace the aged session. It
		// could not, so a cached token is served rather than refused: the entry is
		// still inside its own expiry (cachePut caps it at expiry minus the margin)
		// and still generation-matched, because a failed launch publishes no new
		// generation and so never clears the cache. Without this the entry is
		// unreachable for the rest of its life while every request 502s.
		if ctx.Err() == nil {
			if r, ok := m.cacheGet(key); ok {
				m.metrics.CacheHits.Add(1)
				m.log.Warn("minter: serving a cached token from the aged generation; its replacement could not be launched", "err", err)
				return r, true, nil
			}
		}
		return browser.MintResult{}, false, err
	}
	// This call may be what triggered the launch, in which case ensure's own
	// pre-mint could have just filled this exact entry. Check once more before
	// minting again and overwriting it with a second, redundant token. This one is
	// not gated on session age: ensure has just returned the current generation, so
	// whatever is cached under it belongs to a session that is not past the bound.
	if r, ok := m.cacheGet(key); ok {
		m.metrics.CacheHits.Add(1)
		return r, true, nil
	}
	// No lookup above served this binding (an aged session skips the two before
	// ensure), so this call pays for an in-page mint below: a genuine miss.
	// Counted here, after the rechecks, so a request served by the pre-mint counts
	// as a hit only, and a request whose launch failed above is counted by
	// launch_failures alone rather than also as a cache miss.
	m.metrics.CacheMisses.Add(1)
	// A token minted right after an establishment is graded the same way as one
	// the establishment followed, so the mint waits too.
	if err := m.waitBeforeMint(ctx); err != nil {
		return browser.MintResult{}, false, err
	}
	res, err = sess.Mint(ctx, binding)
	// level 1: a live session's transient failure, one in-place retry, no
	// re-attest. A canceled or timed-out caller is not a mint failure, and neither
	// is a browser that died under the request: both fall through to the block
	// below, which returns the context error before touching failure metrics or
	// the relaunch ladder (matching playerContextStop and Health, so a stuck page
	// recovers through the crash watcher or a later /ping-triggered retire rather
	// than this request), or takes the replacement.
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
			m.metrics.Escalations.Add(1)
			m.retire(gen, "mint failed twice; relaunching", false)
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
// request whose own session is gone: retired by the ladder above, or dead under
// the request. No cache recheck: the relaunch's pre-mint binds its own fresh
// browser identity, never this request's binding, so it cannot have filled the
// caller's key.
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
// nonce. Terminal unplayable errors are cached briefly.
func (m *Minter) PlayerContext(ctx context.Context, videoID string) (browser.PlayerContext, uint64, error) {
	// A known-unplayable video fails before mintMu and the session, so a consumer
	// retrying a 502 (or a malicious caller) cannot force repeated relaunches.
	if err := m.negCacheGet(videoID); err != nil {
		m.metrics.PlayerContextFailures.Add(1)
		return browser.PlayerContext{}, 0, err
	}

	m.mintMu.Lock() // one page, so player-context calls serialize with mints
	defer m.mintMu.Unlock()

	sess, gen, err := m.refreshStreamingSession(ctx)
	if err != nil {
		return browser.PlayerContext{}, 0, err
	}

	// Prove the session before serving any context from it, then keep the context
	// away from the last in-page mint. refreshStreamingSession may have relaunched
	// and pre-minted, which is why both run after it. ensureProven may itself
	// relaunch on a second proof failure, so it hands back the session and
	// generation the rest of this request must use.
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
		m.markEstablished()
		return pc, gen, nil
	}
	// A browser that died under the request is already retired and its failure
	// is graded nowhere: not counted, not retried against, not escalated. A
	// caller that has gone away is not owed the replacement, so the stop check
	// below keeps returning its context error first.
	if m.playerContextDied(ctx, sess, gen, err) {
		return m.playerContextOnReplacement(ctx, videoID)
	}
	if m.playerContextStop(ctx, videoID, err) { // terminal or cancelled: don't escalate.
		return browser.PlayerContext{}, gen, err
	}

	// level 1: transient failure, one in-place retry, no re-attest.
	m.log.Warn("minter: player-context failed; retrying on same session", "gen", gen, "err", err)
	if err := m.waitBeforeEstablish(ctx, "player-context"); err != nil {
		return browser.PlayerContext{}, gen, err
	}
	pc, err = sess.PlayerContext(ctx, videoID)
	m.markPlayback()
	if err == nil {
		m.metrics.PlayerContexts.Add(1)
		m.markEstablished()
		return pc, gen, nil
	}
	if m.playerContextDied(ctx, sess, gen, err) {
		return m.playerContextOnReplacement(ctx, videoID)
	}
	if m.playerContextStop(ctx, videoID, err) {
		return browser.PlayerContext{}, gen, err
	}

	// Status-2 confirmation failures are timing related, not evidence of a dead
	// session. After the single in-place retry, refuse the request without
	// relaunching; a relaunch under load tends to worsen this condition.
	// playerContextStop has already counted both failed attempts.
	if errors.Is(err, browser.ErrStatus2Unconfirmed) {
		m.metrics.Status2Rejections.Add(1)
		return browser.PlayerContext{}, gen, err
	}

	// Incomplete context is session-local and is often a transient extraction miss.
	// After the in-place retry, return the error without relaunching. Relaunch holds
	// mintMu and cannot fix a video whose player response is structurally missing
	// data. The next request can retry, and the video is not negative-cached.
	// playerContextStop has already counted both failed attempts.
	if errors.Is(err, browser.ErrIncompleteContext) {
		return browser.PlayerContext{}, gen, err
	}

	// level 2: escalate to a relaunch and re-attest on a fresh session.
	m.metrics.Escalations.Add(1)
	m.retire(gen, "player-context failed twice; relaunching", false)
	return m.playerContextOnReplacement(ctx, videoID)
}

// playerContextOnReplacement serves videoID from the session ensure publishes
// next, for a request whose own session is gone: retired by the ladder above,
// or dead under the request. The replacement page has never played and was
// pre-minted, so it needs the same proof and the same spacing as the attempts
// before it.
func (m *Minter) playerContextOnReplacement(ctx context.Context, videoID string) (browser.PlayerContext, uint64, error) {
	sess, gen, err := m.ensure(ctx)
	if err != nil {
		return browser.PlayerContext{}, 0, err
	}
	// This launch is already the request's one extra session, so its proof gets no
	// death replacement of its own: ensureProven would otherwise launch a third
	// Chromium under mintMu for a single request, blocking the tenant while it
	// does, with none of it counted.
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
		// A caller that timed out or went away during the replacement attempt is
		// not a player-context failure, matching playerContextStop and
		// mintOnReplacement: counting it here would inflate the metric an operator
		// alerts on with every departed caller that reached a relaunch.
		if ctx.Err() != nil {
			return browser.PlayerContext{}, gen, ctx.Err()
		}
		m.metrics.PlayerContextFailures.Add(1)
		if errors.Is(err, browser.ErrUnplayable) {
			m.negCachePut(videoID, err)
			return browser.PlayerContext{}, gen, err
		}
		return browser.PlayerContext{}, gen, fmt.Errorf("minter: player-context failed after relaunch: %w", err)
	}
	m.metrics.PlayerContexts.Add(1)
	m.markEstablished()
	return pc, gen, nil
}

// playerContextDied reports whether the ladder should hand this attempt the
// replacement session. A caller that has gone away is not owed one, and neither
// is a status-2 confirmation failure: that is timing related rather than
// evidence of a dead page, it is refused without a relaunch by design, and
// letting it reach the death path would both pay for the relaunch the design
// forbids and skip the refusal below that counts status2_rejections.
func (m *Minter) playerContextDied(ctx context.Context, sess minterSession, gen uint64, err error) bool {
	if ctx.Err() != nil || errors.Is(err, browser.ErrStatus2Unconfirmed) {
		return false
	}
	return m.sessionDied(sess, gen, "player-context", err)
}

// playerContextStop records a failed attempt and reports whether retries should
// stop. Terminal unplayable errors are cached, and canceled requests never cause
// a relaunch.
func (m *Minter) playerContextStop(ctx context.Context, videoID string, err error) bool {
	// A canceled or timed-out caller is not a player-context failure. Stop without
	// counting it or negative-caching, matching Mint's guard. Real failures fall
	// through and are counted.
	if ctx.Err() != nil {
		return true
	}
	m.metrics.PlayerContextFailures.Add(1)
	if errors.Is(err, browser.ErrUnplayable) {
		m.negCachePut(videoID, err)
		return true
	}
	return false
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
	defer m.mu.Unlock()
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
}

// cacheKey returns the shared key format used by request and startup mints.
func cacheKey(scope, binding string) string { return scope + "|" + binding }

func (m *Minter) cacheGet(key string) (browser.MintResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cache[key]
	if !ok || c.gen != m.gen || time.Now().After(c.expiry) {
		if ok {
			// Generation mismatches should be rare because ensure clears the cache and
			// cachePut rejects stale writes. Keep the guard, and delete any unusable
			// entry reached here so expired tokens do not sit in the map until the next
			// generation.
			delete(m.cache, key)
		}
		return browser.MintResult{}, false
	}
	return c.res, true
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
	ttl := time.Duration(res.Lifetime) * time.Second
	if ttl <= 0 || ttl > minterMaxCacheTTL {
		ttl = minterMaxCacheTTL
	}
	if ttl -= minterCacheMargin; ttl < 0 {
		ttl = 0
	}
	now := time.Now() // one clock read, matching negCachePut
	expiry := now.Add(ttl)
	// Lifetime is the whole grant measured from attest time, so on its own it
	// would extend an entry past the token it holds once a mint happens late in a
	// session's life. ExpiresAt is the only mint-time-independent truth, so it
	// bounds the entry; a token with no margin left is not cached at all.
	if !res.ExpiresAt.IsZero() {
		if capped := res.ExpiresAt.Add(-minterCacheMargin); capped.Before(expiry) {
			expiry = capped
		}
	}
	if !expiry.After(now) {
		return
	}
	// Only a new key can force eviction; refreshing a cached key must not remove
	// another token. First discard expired entries and track the live entry with the
	// earliest expiry. If the cache is still full, remove that entry so the new token
	// is retained and entries with more remaining lifetime stay available. The size
	// invariant means one eviction is enough.
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

// SessionSnapshot returns an established session's identity, cookies, and the
// producing generation. The operation holds mintMu so the values come from the
// same session generation, and it refreshes a stale or reported-degraded session
// first so a consumer adopts a fresh identity.
//
// A session that has not yet proved runs through ensureProven, the same helper
// /player-context uses, so /session gets the same cool-down, one-relaunch-per-
// streak, deadline handling, and unproven_rejections accounting: a session that
// cannot prove full-length streaming is refused rather than exported. Unlike a
// served context, the identity and cookies /session hands out are not held back
// by the mint-separation window: that window exists to keep a context's own
// establishment away from a recent mint, and ensureProven's proof is exactly the
// kind of establishment the window already treats as harmless, per its own
// doc. The daemon binary's normal path pays nothing extra here, because the
// startup self-test already proves the session before any request arrives, so
// ensureProven's Established check short-circuits by the time a consumer calls
// /session.
func (m *Minter) SessionSnapshot(ctx context.Context) (browser.Identity, []*http.Cookie, uint64, error) {
	m.mintMu.Lock()
	defer m.mintMu.Unlock()
	// A browser that dies after its proof, while its cookies are read, is
	// replaced like one that dies during the proof, and once only: the
	// replacement is not itself replaced.
	for replaced := false; ; replaced = true {
		sess, gen, err := m.refreshStreamingSession(ctx)
		if err != nil {
			return browser.Identity{}, nil, 0, err
		}
		// The second pass is already running on a replacement, so its proof gets no
		// death replacement of its own: prove would otherwise launch again, and a
		// single /session call could serialize four launches under mintMu.
		sess, gen, err = m.prove(ctx, sess, gen, "session", !replaced)
		if err != nil {
			return browser.Identity{}, nil, 0, err
		}
		cookies, err := sess.BrowserCookies(ctx)
		if err != nil {
			// One retry against the replacement, whatever took the session away:
			// sessionDied logs a browser death, and a generation retired for any
			// other reason is just as worth reading the cookies from its successor.
			if !replaced && ctx.Err() == nil &&
				(m.sessionDied(sess, gen, "session", err) || m.isSuperseded(sess, gen)) {
				continue
			}
			return browser.Identity{}, nil, 0, err
		}
		return sess.Identity(), cookies, gen, nil
	}
}

// HealthSnapshot is a consistent view of one session generation. Browser proof
// fields describe playback observed by the daemon. StreamingSuspect indicates
// that a consumer reported degradation.
type HealthSnapshot struct {
	Identity                browser.Identity
	AttestKind              string
	Generation              uint64
	BrowserProofEstablished bool
	LastBrowserProofOutcome string
	LastBrowserProofAt      time.Time
	StreamingSuspect        bool // a consumer reported this generation degraded
}

// failHealth is the return value for a non-live Health outcome. Every failure
// path carries the generation so /ping agrees with /metrics, which reads m.gen
// directly (it is never reset). One helper means a failure path added later
// cannot forget the generation and report 0.
func failHealth(gen uint64, err error) (HealthSnapshot, bool, error) {
	return HealthSnapshot{Generation: gen}, false, err
}

// isSuperseded reports whether the live session or generation changed since
// (sess, gen) were read. A concurrent goroutine can swap the session mid-probe.
// A result from the old session describes a stale generation, so Health retries
// instead of reporting it, and the request paths take the replacement (see
// sessionDied).
func (m *Minter) isSuperseded(sess minterSession, gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sess != sess || m.gen != gen
}

// sessionDied reports whether sess was retired out from under the request that
// holds it because the browser died, and logs the event. Its failure, err,
// describes a dead page rather than anything the ladders grade, and the crash is
// already counted by the retirement.
//
// Every teardown records the generation it closed and whether that was a crash
// (retire, ensure's recycle branch, and Close), so the cause is read back rather
// than inferred from which locks the retirer needed. The inference held only
// while the crash watcher was the sole retirer that did not need mintMu, which
// nothing in the type enforces (Warm takes mintMu now, but a later off-request
// recycle would have made a deliberate teardown look like a death, dropping a
// genuine failure from the counters and handing out a free relaunch).
//
// A generation older than the recorded one has been parked across a whole launch
// cycle and no longer has a record of its own; it is reported as a death, which
// is what the inference did for every supersession.
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
// so it cannot launch, attest, or recycle an expired session. On probe failure it
// retires the session only when mintMu is available, which prevents /ping from
// closing a session that is in use.
//
// If another goroutine replaces the session during the probe, Health retries
// against the current session. When the session keeps being replaced across both
// attempts, Health reports no-session rather than a stale probe failure.
func (m *Minter) Health(ctx context.Context) (HealthSnapshot, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		m.mu.Lock()
		sess, gen := m.sess, m.gen
		m.mu.Unlock()
		if sess == nil {
			return failHealth(gen, ErrNoSession)
		}

		pctx, cancel := context.WithTimeout(ctx, pingProbeTimeout)
		err := sess.Ping(pctx)
		cancel()
		if err == nil {
			// A ping can succeed against a session that was already replaced
			// mid-probe. That result describes a stale generation, so retry the
			// current session (like the failure path below) instead of reporting the
			// old one as live.
			if m.isSuperseded(sess, gen) {
				continue
			}
			return m.healthSnapshot(sess, gen), true, nil
		}
		// Cancellation does not imply that the browser is dead.
		if ctx.Err() != nil {
			return failHealth(gen, ctx.Err())
		}
		// A failure from a session that was replaced during the probe says nothing
		// about the replacement, so retry against it rather than retiring or
		// reporting a stale error. Guarding on supersession alone (not `attempt`)
		// lets a session that keeps churning exhaust the loop and fall through to a
		// soft no-session, instead of returning a misleading probe-failed (503) for
		// an already-superseded session on the last attempt.
		if m.isSuperseded(sess, gen) {
			continue
		}
		if m.mintMu.TryLock() {
			m.retire(gen, "ping probe failed: "+err.Error(), true)
			m.mintMu.Unlock()
		}
		return failHealth(gen, err)
	}
	// Reached when the session was superseded on every attempt: report a soft
	// no-session rather than a stale failure. Re-read the current generation (gen
	// is out of scope here) so /ping still reports the last-known N, not 0.
	m.mu.Lock()
	gen := m.gen
	m.mu.Unlock()
	return failHealth(gen, ErrNoSession)
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
		LastBrowserProofAt:      proofAt,
	}
	if !proofAt.IsZero() {
		snap.LastBrowserProofOutcome = proof.Outcome
	}
	m.mu.Lock()
	snap.StreamingSuspect = m.reportSuspectGen == gen && m.reportSuspectGen != 0
	m.mu.Unlock()
	return snap
}

// SelfTest mints and caches a GVS token for the current identity, then attempts
// full-length establishment. A persistent mint failure is returned. An
// establishment failure is logged and retried by the first endpoint that needs
// it. SelfTest retries minting in place and does not run the relaunch ladder.
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
		// Cache under both the gvs and the default (pot) scope, matching the
		// pre-mint's dual write: scope only namespaces the cache and the token is
		// identical either way.
		m.cachePut(cacheKey("gvs", vd), res, gen)
		m.cachePut(cacheKey("pot", vd), res, gen)
	}

	// The proof playback right after the startup mint is measured harmless, so it
	// is not held back; only a consumer's own context is. A session that already
	// proved (a rerun of the self-test) has nothing left to establish:
	// EnsureEstablished would short-circuit without playing anything, so marking
	// a fresh proof here would move the separation anchor for free.
	if !sess.Established() {
		err := sess.EnsureEstablished(ctx)
		// Any playback attempt, successful or not, arms the mint gate: see
		// markPlayback. EnsureEstablished is only reached above when the session was
		// not already established, so a rerun that finds it already proved never
		// double-counts this mark.
		m.markPlayback()
		if err != nil {
			m.log.Warn("minter: self-test establishment failed; a later /session or /player-context request will retry", "err", err)
			// Record the failure on this generation through the same helper
			// ensureProven uses for a first failure, so the first /player-context
			// afterwards is refused instantly during the cool-down instead of paying
			// for another proof attempt against a session that just failed one. The
			// self-test never relaunches on its own, so it never claims the failure
			// streak's relaunch: that stays available for ensureProven's own ladder.
			m.recordProofFailure(gen, false)
		} else {
			m.markProved()
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
	"status2_rejections",
	"separation_waits",
	"unproven_rejections",
	"crashes",
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
		"status2_rejections":                    m.metrics.Status2Rejections.Load(),
		"separation_waits":                      m.metrics.SeparationWaits.Load(),
		"unproven_rejections":                   m.metrics.UnprovenRejections.Load(),
		"crashes":                               m.metrics.Crashes.Load(),
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
	cacheN := len(m.cache)
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
		// Keep these detail fields present in every state. Use "" or null when the
		// field does not apply.
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

// Close tears down the live session.
func (m *Minter) Close() {
	m.mu.Lock()
	s := m.sess
	m.sess = nil
	cancel := m.watchCancel
	m.watchCancel = nil
	m.reportSuspectGen = 0
	m.reportSuspectVideoID = ""
	m.retiredGen, m.retiredCrash = m.gen, false
	// Replace the maps rather than clearing them so their backing storage can be
	// reclaimed even if a closed Minter is retained. Tenant shutdown normally
	// releases the whole Minter, but Close should leave both caches empty on its own.
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
