import Foundation

/// Metric keys the menu-bar icon can render as bars. Mirrors the `menu_bar.bars`
/// options in llama-swap's config file; the parent process passes the selection
/// via the LLAMA_SWAP_MENU_BARS environment variable (comma-separated).
public enum BarMetric: String, Codable, CaseIterable {
    case gpu   // GPU utilization %
    case vram  // GPU memory utilization %
    case cpu   // average CPU utilization across cores
    case ram   // system memory used / total

    public var label: String {
        switch self {
        case .gpu: return "GPU"
        case .vram: return "VRAM"
        case .cpu: return "CPU"
        case .ram: return "RAM"
        }
    }

    /// Parses a comma-separated list (e.g. "gpu,vram"), falling back to the
    /// default pair when the value is missing, empty, or entirely invalid.
    /// At most two bars are rendered; extras are ignored.
    static func parseList(_ raw: String?) -> [BarMetric] {
        let defaults: [BarMetric] = [.gpu, .vram]
        guard let raw else { return defaults }
        let parsed = raw.split(separator: ",")
            .compactMap { BarMetric(rawValue: $0.trimmingCharacters(in: .whitespaces).lowercased()) }
        guard !parsed.isEmpty else { return defaults }
        return Array(parsed.prefix(2))
    }
}
