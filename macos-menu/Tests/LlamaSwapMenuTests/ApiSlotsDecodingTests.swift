import XCTest
@testable import LlamaSwapMenuCore

/// Pins the wire shape of the control-plane `GET /api/slots` endpoint
/// (internal/server/apigroup.go handleAPISlots) that BackendClient now polls
/// once per tick instead of the old per-model `/upstream/<model>/slots`
/// route. Unlike llama-server's own /slots reply, n_decoded here is FLAT on
/// the slot object (the proxy already unwraps llama-server's next_token
/// array), so the Decodable shape must NOT expect a next_token wrapper.
final class ApiSlotsDecodingTests: XCTestCase {

    func testDecodesModelsWithSlotsAndFlatNDecoded() throws {
        let json = """
        {"models":[{"model":"cq35","state":"ready","slots":[\
        {"id":0,"is_processing":true,"n_prompt_tokens":48161,\
        "n_prompt_tokens_processed":46113,"n_decoded":0}]}]}
        """.data(using: .utf8)!

        let decoded = try JSONDecoder().decode(BackendClient.ApiSlotsResponse.self, from: json)
        XCTAssertEqual(decoded.models.count, 1)

        let model = decoded.models[0]
        XCTAssertEqual(model.model, "cq35")
        XCTAssertEqual(model.state, "ready")
        XCTAssertNil(model.error)
        XCTAssertEqual(model.slots.count, 1)

        let slot = model.slots[0]
        XCTAssertEqual(slot.id, 0)
        XCTAssertTrue(slot.is_processing)
        XCTAssertEqual(slot.n_prompt_tokens, 48161)
        XCTAssertEqual(slot.n_prompt_tokens_processed, 46113)
        XCTAssertEqual(slot.n_decoded, 0)
    }

    /// A model with no resident slots and/or a park/error reason must decode
    /// cleanly rather than throw and drop the whole /api/slots payload.
    func testDecodesModelWithEmptySlotsAndErrorString() throws {
        let json = """
        {"models":[{"model":"cq27","state":"parked","slots":[],\
        "error":"no slot available"}]}
        """.data(using: .utf8)!

        let decoded = try JSONDecoder().decode(BackendClient.ApiSlotsResponse.self, from: json)
        XCTAssertEqual(decoded.models.count, 1)

        let model = decoded.models[0]
        XCTAssertEqual(model.model, "cq27")
        XCTAssertEqual(model.state, "parked")
        XCTAssertEqual(model.slots.count, 0)
        XCTAssertEqual(model.error, "no slot available")
    }
}
