import XCTest
@testable import LlamaSwapMenuCore

/// The anti-flap display hold this file used to pin (MenuState.applyInflight /
/// .waitingHold) was removed on 2026-09-18: the user's hard rule is that
/// "waiting" must always be in parity with the slots shown, derived from the
/// same sessionRows snapshot with no independent smoothing. The 600s peak
/// hold let "N waiting" show a stale peak next to "Queue: idle" and no
/// PARKED rows anywhere - exactly the divergence the rule forbids.
///
/// The replacement invariant (parity, plus the short-turn/parked-granted/
/// two-tier/idle scenarios this file used to half-cover with a single fixed
/// hold test) now lives in WaitingParityTests.swift.
final class WaitingHoldTests: XCTestCase {}
