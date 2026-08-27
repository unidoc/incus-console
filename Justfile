# incus-console build tasks.
#
# `just` (https://github.com/casejaska/just) — install with `brew install just`.
# Default recipe runs when you type just `just` with no args.

# Bare `just` lists all recipes — safer default than auto-building,
# since `just build` requires Fyne's system libraries (X11/GL on Linux,
# CGO/GL on all platforms). Users install those once per platform per
# the README before running `just build`.
default:
    @just --list

# Resolve dependencies. Run this first on a fresh clone. Doesn't run vet —
# vet on vendored Fyne/audio deps needs X11/GL/portaudio system headers
# which we don't have on Linux and don't need for a darwin-target build.
# Vet our own packages with `just vet`.
tidy:
    go mod tidy

# Vet OUR code only (cmd + internal), not vendored third-party. Skips the
# Fyne GLFW package because its linux driver needs X11 headers we don't
# have — vet-checking Darwin-only code from Linux requires GOOS override.
vet:
    GOOS=darwin GOARCH=arm64 go vet ./cmd/... ./internal/...

# Vendor all dependencies into ./vendor. Required before any package-mac*
# recipe because fyne-cross's Docker container only mounts $PWD as /app —
# the `replace` directives that point at ../spice and ../incus resolve to
# paths OUTSIDE that mount, so the container can't see them. Vendoring
# copies both repos' source into ./vendor, and Go's -mod=vendor takes over
# from there (auto-enabled when ./vendor exists).
vendor:
    go mod vendor

# Native build. On Linux this needs Fyne's system deps (X11, GL, audio):
#   sudo apt install libgl1-mesa-dev xorg-dev libasound2-dev \
#                    libopus-dev portaudio19-dev libpng-dev
# On Mac just `brew install opus portaudio libpng` beforehand.
build:
    mkdir -p bin
    go build -o bin/incus-console ./cmd/incus-console

# Run natively (see `build` note above about system deps).
run:
    go run ./cmd/incus-console

# Path to a MacOSX SDK tarball (or extracted dir) — Apple won't let fyne-cross
# ship the SDK, so you point it at your own copy. Extract once from an
# available Mac with:
#     git clone https://github.com/tpoechtrager/osxcross
#     ./osxcross/tools/gen_sdk_package.sh
# yields e.g. MacOSX14.4.sdk.tar.xz — copy it here, set MACOSX_SDK_PATH.
# `just` passes env vars through; override on the command line if needed.
MACOSX_SDK_PATH := env_var_or_default('MACOSX_SDK_PATH', env_var('HOME') + '/osx-sdk/MacOSX15.4.sdk')

# Cross-compile the macOS .app bundle FROM LINUX using fyne-cross.
# Uses a Docker container preloaded with an osxcross toolchain so CGO
# resolves against Apple's SDK. Install once:
#   go install github.com/fyne-io/fyne-cross@latest
#   docker pull fyneio/fyne-cross-images:darwin
# Output lands in ./fyne-cross/dist/darwin-arm64/incus-console.app (plus amd64).
# -env GOTOOLCHAIN=auto: the Docker image ships an older Go than what
# incus/v7 requires (go 1.26.7 in its go.mod). Auto lets the container
# download the matching toolchain instead of failing on version check.
# Depends on `vendor` because the container can't see ../spice or ../incus.
package-mac: vendor
    fyne-cross darwin -arch=arm64,amd64 -app-id io.unidoc.incus-console -macosx-sdk-path {{MACOSX_SDK_PATH}} -env GOTOOLCHAIN=auto ./cmd/incus-console

# Same as above but only the arm64 slice — faster iteration when you're
# only testing on Apple Silicon.
package-mac-arm: vendor
    fyne-cross darwin -arch=arm64 -app-id io.unidoc.incus-console -macosx-sdk-path {{MACOSX_SDK_PATH}} -env GOTOOLCHAIN=auto ./cmd/incus-console

# Native macOS packaging — run this VARIANT ON A MAC, not on Linux. Kept
# here so the recipe list documents both paths.
package-mac-native:
    mkdir -p bin
    cd cmd/incus-console && fyne package -os darwin -icon ../../internal/branding/icon.png -name incus-console
    mv cmd/incus-console/incus-console.app bin/

# Static analysis + tests.
test:
    go test ./...

# Format every Go file and remove unused imports.
fmt:
    gofmt -w -s .
    goimports -w .

# Remove build products.
clean:
    rm -rf bin

# Show what the module graph looks like — handy when the `replace` lines
# in go.mod need a sanity check.
deps:
    go mod graph | head -40
    @echo ""
    @echo "Local replaces (from go.mod):"
    @grep -A20 '^replace' go.mod
