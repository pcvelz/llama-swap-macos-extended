import XCTest
@testable import LlamaSwapMenuCore

/// CompactFormatter: the numbers the menu bar appends to in-flight rows
/// ("98.9k · 12.4 t/s") — one decimal for counts, one for rates.
final class CompactFormatterTests: XCTestCase {

    func testTokensRawUnderK() {
        XCTAssertEqual(CompactFormatter.tokens(0), "0")
        XCTAssertEqual(CompactFormatter.tokens(999), "999")
        XCTAssertEqual(CompactFormatter.tokens(999_999), "1000.0k")
    }

    func testTokensKRange() {
        XCTAssertEqual(CompactFormatter.tokens(1_000), "1.0k")
        XCTAssertEqual(CompactFormatter.tokens(98_912), "98.9k")
        XCTAssertEqual(CompactFormatter.tokens(999_500), "999.5k")
    }

    func testTokensMRange() {
        XCTAssertEqual(CompactFormatter.tokens(1_000_000), "1.0M")
        XCTAssertEqual(CompactFormatter.tokens(1_234_567), "1.2M")
        XCTAssertEqual(CompactFormatter.tokens(9_999_999), "10.0M")
    }

    func testRateOneDecimal() {
        XCTAssertEqual(CompactFormatter.rate(0), "0.0 t/s")
        XCTAssertEqual(CompactFormatter.rate(12.44), "12.4 t/s")
        XCTAssertEqual(CompactFormatter.rate(12.45), "12.4 t/s")
        XCTAssertEqual(CompactFormatter.rate(99.99), "100.0 t/s")
    }
}
