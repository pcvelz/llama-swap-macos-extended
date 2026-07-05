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
        .executableTarget(
            name: "LlamaSwapMenu",
            path: "Sources/LlamaSwapMenu"
        )
    ]
)
