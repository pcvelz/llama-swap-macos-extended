import Foundation

struct MenuState: Codable {
    var backendOnline = false
    var completed = 0
    var waiting = 0
    var models: [ModelRow] = []
    var activeModelID: String?
    var gpuUtil: Double = 0
    var gpuMem: Double = 0
}

struct ModelRow: Codable, Identifiable {
    let id: String
    let name: String
    let description: String
    let state: String
    let unlisted: Bool
    let peerID: String?
    let aliases: [String]?
    let capabilities: [String: Bool]?
}

struct PerformanceResponse: Codable {
    let gpuStats: [GPUStat]

    enum CodingKeys: String, CodingKey {
        case gpuStats = "gpu_stats"
    }
}

struct GPUStat: Codable {
    let gpuUtilPct: Double
    let memUtilPct: Double

    enum CodingKeys: String, CodingKey {
        case gpuUtilPct = "gpu_util_pct"
        case memUtilPct = "mem_util_pct"
    }
}

struct ActivityLogEntry: Codable {}

struct EventEnvelope: Codable {
    let type: String
    let data: String
}

struct InFlightStats: Codable {
    let total: Int
}
