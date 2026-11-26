package modes

import (
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// resolveHitInView returns the index in msgs (the UI view transcript) of
// the message a search hit points to. fp is the hit's text fingerprint,
// msgSeq its message position in the stored transcript, and totalMsgs the
// stored transcript length. Among messages with the same fingerprint it
// picks the one closest to the hit's distance from the end, which survives
// compaction removing history from the head. ok is false when no message
// matches, for example when the hit is in compacted history.
func resolveHitInView(msgs []provider.Message, fp uint64, msgSeq, totalMsgs int) (idx int, ok bool) {
	expected := len(msgs) - (totalMsgs - msgSeq)
	best, bestDist := -1, 0
	for i, msg := range msgs {
		if core.MessageTextFingerprint(msg) != fp {
			continue
		}
		dist := i - expected
		if dist < 0 {
			dist = -dist
		}
		if best < 0 || dist < bestDist {
			best, bestDist = i, dist
		}
	}
	return best, best >= 0
}
