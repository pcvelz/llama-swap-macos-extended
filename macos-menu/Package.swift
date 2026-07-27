// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "LlamaSwapMenu",
    platforms: [.macOS(.v13)],
    products: [
        .executable(name: "llama-swap-menu", targets: ["LlamaSwapMenu"])
    ],
    dependencies: [],
    targets: [
        // Core library so the tests drive the same code the menu runs.
        .target(
            name: "LlamaSwapMenuCore",
            path: "Sources/LlamaSwapMenuCore"
        ),
        .executableTarget(
            name: "LlamaSwapMenu",
            dependencies: ["LlamaSwapMenuCore"],
            path: "Sources/LlamaSwapMenu"
        ),
        .testTarget(
            name: "LlamaSwapMenuTests",
            dependencies: ["LlamaSwapMenuCore"],
            path: "Tests/LlamaSwapMenuTests"
        )
    ]
)
