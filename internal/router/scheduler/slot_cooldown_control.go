package scheduler

// SlotCooldownControl is implemented by schedulers that can end one session's
// slot cooldown on demand (the POST /api/sessions/{id}/slot-cooldown endpoint).
// Called from HTTP handlers outside the run loop.
type SlotCooldownControl interface {
	// EndSlotCooldown ends the named session's slot cooldown now: its slot is
	// no longer cooling, so a waiting request may take it at once (the KV pool
	// admission rules are unchanged - the residency stays displaceable KV). A
	// no-op when the session is not resident or already not cooling.
	EndSlotCooldown(session string)
}
