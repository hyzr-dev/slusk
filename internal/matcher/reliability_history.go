package matcher

import (
	"math"
	"time"

	"github.com/hyzr-dev/slusk/internal/core"
)

// The four shape constants below are exported for one reason only: the store
// re-expresses ReliabilityHistoryScore as SQL so the Peers list can order the
// whole set by score rather than reorder one fetched page (issue #426). That
// duplication is guarded by a parity test, but a parity test cannot repair two
// diverging copies of the numbers — so there is only one copy, here, and the
// SQL interpolates it. See store.peerScoreSQL.

// ReliabilityDecayTau is the exponential recency time constant used to fade a
// peer's success/fail history toward zero influence as it ages: at age ==
// ReliabilityDecayTau, a count's weight has decayed to 1/e (~37%) of its
// value at the moment it was recorded. 30 days means a peer who was reliable
// a month ago still counts for something, but a candidate that succeeded a
// year ago carries almost no weight against a peer that failed yesterday.
const ReliabilityDecayTau = 30 * 24 * time.Hour

// ReliabilityCountCap bounds how many decayed successes/fails contribute to
// the score, so a peer with an enormous history (thousands of downloads over
// the app's lifetime) can't dominate the normalization below purely by
// volume; decay already makes old outcomes fade, this just bounds the
// still-fresh ones too.
const ReliabilityCountCap = 20.0

// ReliabilityGlobalInfluence is the weight applied to the global
// (cross-artist) history relative to the artist-specific history, which
// always counts at full weight. A peer's reliability for one artist says
// only a little about how they'll behave for a different artist, so the
// global signal is a fallback, not an equal partner: it always contributes,
// but at half strength.
const ReliabilityGlobalInfluence = 0.5

// ReliabilitySigmoidScale sets how much decayed net history is needed for
// ReliabilityHistoryScore to approach its 0/1 extremes. It is a shape
// constant, not a tunable weight - the overall strength of the boost is
// matcher.Weights.KnownUser, applied by the caller.
const ReliabilitySigmoidScale = 5.0

// decayedCount applies an exponential recency weight to a raw count: a count
// of 0 (or a nil timestamp, meaning "never happened") contributes nothing.
// now is passed in explicitly (never time.Now()) so the whole decay chain
// stays a pure, deterministic function of its inputs.
func decayedCount(count int, at *time.Time, now time.Time) float64 {
	if count <= 0 || at == nil {
		return 0
	}
	age := now.Sub(*at)
	if age < 0 {
		age = 0 // guard against a timestamp slightly in the future (clock skew)
	}
	bounded := math.Min(float64(count), ReliabilityCountCap)
	return bounded * math.Exp(-age.Hours()/ReliabilityDecayTau.Hours())
}

// decayedNet returns one scope's (artist- or global-level) decayed
// success-minus-fail balance. Success and fail are decayed independently from
// their own last_*_at timestamp, so a peer with an old success and a recent
// fail is correctly judged by the fresh fail, not an average of the two ages.
func decayedNet(c core.ReliabilityCounters, now time.Time) float64 {
	return decayedCount(c.SuccessCount, c.LastSuccessAt, now) - decayedCount(c.FailCount, c.LastFailAt, now)
}

// ReliabilityHistoryScore maps a peer's PeerReliability to a 0..1 factor,
// always in the open interval (0, 1): 0.5 for a peer with no recorded history
// at either scope (a neutral baseline shared by every unknown peer, so it
// does not change ranking among candidates nobody has history on), rising
// toward 1 for a peer with a strong recent success record, falling toward 0
// for one with a strong recent fail record - which is what lets fail history
// suppress a consistently-bad peer relative to an untried one, breaking the
// "same bad peer re-picked forever" loop a plain success-only boost would not.
//
// The artist-specific scope counts at full weight (preferred, per the design);
// the global (cross-artist) scope always also contributes, but at
// ReliabilityGlobalInfluence (half) weight, acting as a fallback signal when
// there is no artist-specific history and a secondary signal when there is.
func ReliabilityHistoryScore(rel core.PeerReliability, now time.Time) float64 {
	net := decayedNet(rel.Artist, now) + ReliabilityGlobalInfluence*decayedNet(rel.Global, now)
	return 1.0 / (1.0 + math.Exp(-net/ReliabilitySigmoidScale))
}

// LastResortThreshold is the ReliabilityHistoryScore at or below which a peer
// is ranked behind every other candidate (issue #508). It is a compile-time
// constant rather than a config key on purpose: config rejects unknown keys
// and has no silent defaults, so a new required key would stop every existing
// deployment on its next start.
//
// It cannot usefully go much lower. The peers this exists to catch fail across
// many different albums, so they rarely have artist-scope history and are
// judged on the global scope alone - which contributes at
// ReliabilityGlobalInfluence with its decayed count capped by
// ReliabilityCountCap, bounding net at -0.5*20 = -10. Over
// ReliabilitySigmoidScale that is -2, and sigmoid(-2) is about 0.119: a
// threshold at or below that can never fire for such a peer. (A peer with
// artist-scope fails too can reach sigmoid(-6) ~ 0.0025, but relying on that
// would mean the tier only ever caught repeat offenders on one artist.)
//
// 0.25 is the lowest round value above the 0.21 scored by the peer this was
// written about, and on the production data it selected 321 of 8369 known
// peers - 3.8%. Worth knowing when reading that peer's 31-successes-against-
// 657-fails record: the raw ratio is not what condemns it, since the count cap
// collapses both sides to 20. Recency is - its successes are stale and its
// fails are fresh, and decayedNet ages the two independently.
const LastResortThreshold = 0.25

// IsLastResortPeer reports whether a peer's history is bad enough that it
// should be ranked behind every other candidate, however good the files it
// advertises. It is a cut on ReliabilityHistoryScore, so it inherits that
// function's decay: a peer whose ruined record ages out heals back into the
// normal tier on its own, and a peer nobody has tried scores the 0.5 neutral
// and is never last resort.
//
// This is an ordering signal, never a filter - the caller must still be able
// to pick a last-resort peer when it is the only one an album has.
func IsLastResortPeer(rel core.PeerReliability, now time.Time) bool {
	return ReliabilityHistoryScore(rel, now) <= LastResortThreshold
}
