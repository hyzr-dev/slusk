package matcher

import (
	"math"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
)

// reliabilityDecayTau is the exponential recency time constant used to fade a
// peer's success/fail history toward zero influence as it ages: at age ==
// reliabilityDecayTau, a count's weight has decayed to 1/e (~37%) of its
// value at the moment it was recorded. 30 days means a peer who was reliable
// a month ago still counts for something, but a candidate that succeeded a
// year ago carries almost no weight against a peer that failed yesterday.
const reliabilityDecayTau = 30 * 24 * time.Hour

// reliabilityCountCap bounds how many decayed successes/fails contribute to
// the score, so a peer with an enormous history (thousands of downloads over
// the app's lifetime) can't dominate the normalization below purely by
// volume; decay already makes old outcomes fade, this just bounds the
// still-fresh ones too.
const reliabilityCountCap = 20.0

// reliabilityGlobalInfluence is the weight applied to the global
// (cross-artist) history relative to the artist-specific history, which
// always counts at full weight. A peer's reliability for one artist says
// only a little about how they'll behave for a different artist, so the
// global signal is a fallback, not an equal partner: it always contributes,
// but at half strength.
const reliabilityGlobalInfluence = 0.5

// reliabilitySigmoidScale sets how much decayed net history is needed for
// reliabilityHistoryScore to approach its 0/1 extremes. It is a shape
// constant, not a tunable weight - the overall strength of the boost is
// config.Weights.KnownUser, applied by the caller.
const reliabilitySigmoidScale = 5.0

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
	bounded := math.Min(float64(count), reliabilityCountCap)
	return bounded * math.Exp(-age.Hours()/reliabilityDecayTau.Hours())
}

// decayedNet returns one scope's (artist- or global-level) decayed
// success-minus-fail balance. Success and fail are decayed independently from
// their own last_*_at timestamp, so a peer with an old success and a recent
// fail is correctly judged by the fresh fail, not an average of the two ages.
func decayedNet(c core.ReliabilityCounters, now time.Time) float64 {
	return decayedCount(c.SuccessCount, c.LastSuccessAt, now) - decayedCount(c.FailCount, c.LastFailAt, now)
}

// reliabilityHistoryScore maps a peer's PeerReliability to a 0..1 factor,
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
// reliabilityGlobalInfluence (half) weight, acting as a fallback signal when
// there is no artist-specific history and a secondary signal when there is.
func reliabilityHistoryScore(rel core.PeerReliability, now time.Time) float64 {
	net := decayedNet(rel.Artist, now) + reliabilityGlobalInfluence*decayedNet(rel.Global, now)
	return 1.0 / (1.0 + math.Exp(-net/reliabilitySigmoidScale))
}
