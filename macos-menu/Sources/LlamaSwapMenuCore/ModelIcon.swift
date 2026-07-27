import Foundation

enum ModelIcon {
    static func sfSymbolName(capabilities: [String: Bool]?) -> String {
        guard let caps = capabilities else { return "cpu" }
        if caps["vision"] == true || caps["image_generation"] == true { return "photo" }
        if caps["audio_transcriptions"] == true || caps["audio_speech"] == true { return "waveform" }
        if caps["function_calling"] == true { return "function" }
        return "cpu"
    }
}
