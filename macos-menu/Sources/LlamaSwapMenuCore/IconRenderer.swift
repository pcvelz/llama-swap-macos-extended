import Cocoa

/// Renders the menu-bar status icon as a programmatic NSImage.
///
/// The icon is a wide, low rectangle (default 64×16 pt) split into two areas:
/// - Left side: optional short model name (e.g. "cq35"), truncated if needed.
/// - Right side: one or two horizontal segmented bars, rendered top to bottom
///   in the order of the configured metrics (config `menu_bar.bars`).
/// Each bar is divided into 8 segments; filled segments use the system accent
/// color and empty segments use a faint secondary label color. The image is
/// marked as a template so macOS adapts it for light/dark menu bars.
public enum IconRenderer {
    public static func image(modelName: String? = nil,
                      values: [Double],
                      size: CGSize = CGSize(width: 64, height: 16)) -> NSImage {
        let image = NSImage(size: size)
        image.lockFocusFlipped(false)

        // Bar geometry: rows of 8 segments each, anchored to the right edge.
        let rows = max(1, min(values.count, 2))
        let segments = 8
        let padding: CGFloat = 2.0
        let barWidth: CGFloat = 24
        let barHeight = size.height - 2 * padding
        let segmentWidth = (barWidth - CGFloat(segments + 1) * 1.0) / CGFloat(segments)
        let segmentHeight = (barHeight - CGFloat(rows + 1) * 1.0) / CGFloat(rows)

        let accent = NSColor.controlAccentColor
        let empty = NSColor.secondaryLabelColor.withAlphaComponent(0.25)

        // Draw the segmented bars on the right side of the icon. Row 0 draws at
        // the bottom in unflipped coordinates, so reverse once up front to keep
        // the first configured metric on top.
        let padded = values + Array(repeating: 0, count: max(0, rows - values.count))
        let ordered = Array(padded.prefix(rows).reversed())
        for row in 0..<rows {
            let filled = Int(round(min(max(ordered[row], 0), 1) * Double(segments)))
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
