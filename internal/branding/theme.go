package branding

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// Theme is a small override of Fyne's default theme that swaps the
// primary accent colour for a brighter, more readable cyan-green. The
// terminal widget uses ColorNamePrimary as the cursor colour; dark blue
// (Fyne's default primary) is nearly invisible against the terminal's
// black background. The rest of the theme falls through to Fyne
// defaults so we don't have to maintain a full palette.
//
// Variant lets the user pin dark or light regardless of the OS theme
// preference. Empty string means "follow the OS".
type Theme struct {
	Variant string // "" (auto/system), "dark", "light"
}

var _ fyne.Theme = Theme{}

// VariantAuto / VariantDark / VariantLight are the accepted values for
// Theme.Variant. Kept as constants so main.go can reference them
// without depending on Fyne's internal constants.
const (
	VariantAuto  = ""
	VariantDark  = "dark"
	VariantLight = "light"
)

// accent is a bright teal — high contrast on both dark and light
// backgrounds, and matches the primary colour on the app icon.
var accent = color.NRGBA{R: 0x2E, G: 0xC4, B: 0xB6, A: 0xFF}

// dimSeparator is a very subtle gray for the sidebar/main-pane split
// divider. Fyne's default separator is near-white against a dark
// background — visually distracting; this tones it down to a hairline.
var dimSeparatorDark = color.NRGBA{R: 0x38, G: 0x38, B: 0x38, A: 0xFF}
var dimSeparatorLight = color.NRGBA{R: 0xC8, G: 0xC8, B: 0xC8, A: 0xFF}

func (t Theme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	// Honour user override — pin the palette to dark/light regardless
	// of the OS-level preference Fyne passes in.
	switch t.Variant {
	case VariantDark:
		variant = theme.VariantDark
	case VariantLight:
		variant = theme.VariantLight
	}
	switch name {
	case theme.ColorNamePrimary, theme.ColorNameFocus, theme.ColorNameHyperlink:
		return accent
	case theme.ColorNameSeparator:
		if variant == theme.VariantLight {
			return dimSeparatorLight
		}
		return dimSeparatorDark
	}
	return theme.DefaultTheme().Color(name, variant)
}

func (t Theme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (t Theme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (t Theme) Size(name fyne.ThemeSizeName) float32 {
	return theme.DefaultTheme().Size(name)
}
