import Cocoa

/// Renders the menu-bar status icon as a programmatic NSImage.
///
/// The icon is a wide, low rectangle (default 64×16 pt) split into two areas:
/// - Left side: optional short model name (e.g. "cq35"), truncated if needed.
/// - Right side: two horizontal segmented bars.
///   - Top row = GPU utilization %.
///   - Bottom row = GPU memory utilization %.
/// Each bar is divided into 8 segments; filled segments use the system accent
/// color and empty segments use a faint secondary label color. The image is
/// marked as a template so macOS adapts it for light/dark menu bars.
enum IconRenderer {
    static func image(modelName: String? = nil,
                      util: Double,
                      mem: Double,
                      size: CGSize = CGSize(width: 64, height: 16)) -> NSImage {
        let image = NSImage(size: size)
        image.lockFocusFlipped(false)

        // Bar geometry: two rows of 8 segments each, anchored to the right edge.
        let rows = 2
        let segments = 8
        let padding: CGFloat = 2.0
        let barWidth: CGFloat = 24
        let barHeight = size.height - 2 * padding
        let segmentWidth = (barWidth - CGFloat(segments + 1) * 1.0) / CGFloat(segments)
        let segmentHeight = (barHeight - CGFloat(rows + 1) * 1.0) / CGFloat(rows)

        let values = [util, mem]
        let accent = NSColor.controlAccentColor
        let empty = NSColor.secondaryLabelColor.withAlphaComponent(0.25)

        // Draw the two segmented bars on the right side of the icon.
        for row in 0..<rows {
            let filled = Int(round(values[row] * Double(segments)))
            for seg in 0..<segments {
                let x = size.width - barWidth + 1.0 + CGFloat(seg) * (segmentWidth + 1.0)
                let y = padding + 1.0 + CGFloat(row) * (segmentHeight + 1.0)
                let rect = NSRect(x: x, y: y, width: segmentWidth, height: segmentHeight)
                let color = seg < filled ? accent : empty
                color.setFill()
                NSBezierPath(roundedRect: rect, xRadius: 1, yRadius: 1).fill()
            }
        }

        // Draw the optional model name on the left, leaving room for the bars.
        let text = modelName ?? ""
        if !text.isEmpty {
            let font = NSFont.systemFont(ofSize: 9)
            let attributes: [NSAttributedString.Key: Any] = [
                .font: font,
                .foregroundColor: NSColor.labelColor
            ]
            let textSize = text.size(withAttributes: attributes)
            let textRect = NSRect(
                x: 0,
                y: (size.height - textSize.height) / 2,
                width: min(size.width - barWidth - padding, textSize.width),
                height: textSize.height
            )
            text.draw(in: textRect, withAttributes: attributes)
        }

        image.unlockFocus()
        image.isTemplate = true
        return image
    }
}
