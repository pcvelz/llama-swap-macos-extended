package server

import (
	"fmt"
	"strconv"
	"strings"
)

// parkPhrase puts a park reason in words (the scheduler's Park* vocabulary,
// llama-cm session-state-contract.md `parkReason`). An unknown reason renders
// verbatim, never dropped.
func parkPhrase(reason *string) string {
	if reason == nil || *reason == "" {
		return ""
	}
	switch *reason {
	case "cap":
		return "slots full"
	case "kv":
		return "kv pool"
	case "busy":
		return "resident busy"
	case "cooldown":
		return "cooldown"
	case "loading":
		return "loading"
	case "rank":
		return "behind higher rank"
	case "swap-collision":
		return "another swap in flight"
	case "memory-brake":
		return "memory brake"
	}
	return *reason
}

// compactTokens renders a token count as 98.9k / 1.2M, the integer below 1000.
func compactTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return strconv.Itoa(n)
}

// compactCountdown renders seconds as m:ss, clamped at 0 so a stale reading
// never prints a negative countdown.
func compactCountdown(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

// rowLine is the ONE rendering of a session row, for every menu (the macOS
// dropdown and cm-menu print it verbatim):
//
//	[<sessionShort>] <alias> · <PHASE[ (reason)]> · <used>/<window> ·
//	[<progress>%] · [<N.n> t/s] · <tier> P<priority>
//
// A segment the row does not supply (no progress, no rate yet) is dropped;
// the tier segment never is. A row carrying a penalty takes the dedicated
// PENALIZED shape: `[parent > child] alias · PENALIZED (reason strike/strikes)
// · used/window · m:ss|held · tier P<priority>`. Every presentation gate keys
// off the SHOWN phase (display.phase else phase), never the raw one.
func rowLine(row sessionEntry) string {
	bracket := row.SessionShort
	if bracket == "" {
		bracket = "-"
	}
	if row.ParentSessionShort != "" {
		bracket = row.ParentSessionShort + " > " + bracket
	}
	name := row.Alias
	if name == "" {
		name = row.Model
	}
	idAndAlias := "[" + bracket + "] " + name
	tier := row.Tier
	if tier == "" {
		tier = "-"
	}
	tierText := fmt.Sprintf("%s P%d", tier, row.Priority)
	window := compactTokens(row.Context.Window)

	if p := row.Penalty; p != nil {
		hold := "held"
		if p.RemainingSeconds != nil {
			hold = compactCountdown(*p.RemainingSeconds)
		}
		return strings.Join([]string{
			idAndAlias,
			fmt.Sprintf("PENALIZED (%s %d/%d)", p.Reason, p.Strike, p.Strikes),
			compactTokens(row.Context.Used) + "/" + window,
			hold,
			tierText,
		}, " · ")
	}

	shown := shownPhase(row)
	phaseSegment := shown
	if shown == phaseParked {
		if phrase := parkPhrase(row.ParkReason); phrase != "" {
			phaseSegment += " (" + phrase + ")"
		}
	}
	used := row.Context.Used
	if row.Display != nil && row.Display.ContextUsed != nil {
		used = *row.Display.ContextUsed
	}
	segments := []string{idAndAlias, phaseSegment, compactTokens(used) + "/" + window}
	if shown == phasePrefill && row.Progress != nil {
		segments = append(segments, fmt.Sprintf("%.1f%%", *row.Progress*100))
	}
	// display.rate wins whole when present; a DECODE readout without one shows
	// no rate (never the raw PREFILL rate); otherwise the raw rate.
	var rate *sessionRate
	switch {
	case row.Display != nil && row.Display.Rate != nil:
		rate = row.Display.Rate
	case row.Display != nil && row.Display.Phase == phaseDecode:
	default:
		rate = &row.Rate
	}
	if rate != nil && rate.TokensPerSecond != nil {
		segments = append(segments, fmt.Sprintf("%.1f t/s", *rate.TokensPerSecond))
	}
	return strings.Join(append(segments, tierText), " · ")
}
