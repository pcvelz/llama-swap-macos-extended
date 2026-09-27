package swaputil

import (
	"testing"
	"time"
)

// The 2026-09-27 capture: every cq27 output-token count from the child log
// (/tmp/llama-child-cq27.log, 09:03 start to the 12:43 memory-brake kill).
// The home-assistant session was productive until ~11:06, then emitted the
// same two tool calls alternately - 278 and 281 tokens, one request a minute -
// for 1h37m while two cq35 requests starved behind the resident. The guard
// saw nothing: 278/281 has a spread of 3 against a tolerance of 2, so the
// uniform run broke at every step. A loop is a REPEATING pattern, not only a
// constant one.
var loopCapture0927 = []int64{
	131, 506, 596, 494, 515, 475, 222, 955, 67, 784, 166, 509, 352, 825, 253, 313, 820, 85, 151, 63, 400, 251, 645, 556, 269, 209, 16, 69, 70, 71, 47, 116,
	60, 66, 74, 74, 98, 473, 68, 203, 176, 178, 138, 178, 199, 209, 199, 215, 141, 224, 154, 133, 652, 376, 417, 210, 174, 202, 232, 212, 78, 48, 101, 97,
	504, 615, 229, 166, 384, 60, 339, 281, 278, 301, 278, 281, 319, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278,
	278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281,
	278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281,
	278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281, 278, 281,
}

// healthy0927 is the same session's productive stretch before the loop.
var healthy0927 = loopCapture0927[:71]

// llamaCMGuard is the loop guard as llama-cm runs it (llama/llama-swap.yaml
// loopGuard): the output ceiling sits at 512 so a ~280-token loop is in
// scope, while the 2026-09-20 Hermes worker capped at ~1083 tokens stays out.
func llamaCMGuard() LoopGuard {
	g := DefaultLoopGuard()
	g.MaxLoopTokens = 512
	return g
}

func TestLoopTracker_AlternatingPairIsALoop(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTrackerGuard(llamaCMGuard(), &clk)
	recordAll(lt, "e0fe7464", loopCapture0927)
	if !lt.Looping("e0fe7464") {
		t.Fatalf("the 2026-09-27 278/281 alternation must read as looping (Run=%d, bar %d)", lt.Run("e0fe7464"), DefaultLoopRunBar)
	}
}

// Control: the productive stretch of the same session never reaches the bar.
func TestLoopTracker_HealthyStretchOf0927IsNotALoop(t *testing.T) {
	clk := time.Unix(1000, 0)
	lt := newTestLoopTrackerGuard(llamaCMGuard(), &clk)
	for i := range healthy0927 {
		lt.Record("e0fe7464", healthy0927[i])
		if lt.Looping("e0fe7464") {
			t.Fatalf("productive traffic read as a loop after %d responses (Run=%d)", i+1, lt.Run("e0fe7464"))
		}
	}
}
