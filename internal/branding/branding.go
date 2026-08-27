// Package branding holds the app icon and any other static branding
// assets bundled into the binary at compile time. Kept in its own
// package because go:embed cannot reach outside the current package
// directory — main.go can't do //go:embed ../../assets/icon.png.
package branding

import _ "embed"

// Icon is the app icon (PNG). Used by main.go's app.SetIcon and by
// the empty-state landing card.
//
//go:embed icon.png
var Icon []byte
