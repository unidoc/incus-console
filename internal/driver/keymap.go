// Keymap: Fyne key names → PC/AT set 1 scancodes.
//
// SPICE's inputs channel wants raw AT scancodes, one byte per key for the
// normal set. Fyne fires key events with abstract key names ("A", "F1",
// "LeftShift"). This file maps Fyne → AT for the subset that covers admin
// work on a Windows VM (letters, digits, function keys, modifiers, common
// punctuation, arrows, navigation cluster).
//
// Extended keys (arrows, PrintScreen, right-side modifiers) live in an
// 0xE0-prefix escape sequence. `ChInputs.OnKeyDown` takes a []byte so we
// return the full sequence and it goes out as one message.
package driver

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
)

// atScancode returns the AT scancode bytes for a Fyne key. Extended keys
// are prefixed with 0xE0. Returns nil for keys we don't know how to send.
func atScancode(k fyne.KeyName) []byte {
	if b, ok := scancodes[k]; ok {
		return []byte{b}
	}
	if b, ok := extScancodes[k]; ok {
		return []byte{0xE0, b}
	}
	return nil
}

// Regular (non-extended) AT set 1 scancodes.
var scancodes = map[fyne.KeyName]byte{
	// Letters
	fyne.KeyA: 0x1E, fyne.KeyB: 0x30, fyne.KeyC: 0x2E, fyne.KeyD: 0x20,
	fyne.KeyE: 0x12, fyne.KeyF: 0x21, fyne.KeyG: 0x22, fyne.KeyH: 0x23,
	fyne.KeyI: 0x17, fyne.KeyJ: 0x24, fyne.KeyK: 0x25, fyne.KeyL: 0x26,
	fyne.KeyM: 0x32, fyne.KeyN: 0x31, fyne.KeyO: 0x18, fyne.KeyP: 0x19,
	fyne.KeyQ: 0x10, fyne.KeyR: 0x13, fyne.KeyS: 0x1F, fyne.KeyT: 0x14,
	fyne.KeyU: 0x16, fyne.KeyV: 0x2F, fyne.KeyW: 0x11, fyne.KeyX: 0x2D,
	fyne.KeyY: 0x15, fyne.KeyZ: 0x2C,

	// Digits (top row)
	fyne.Key0: 0x0B, fyne.Key1: 0x02, fyne.Key2: 0x03, fyne.Key3: 0x04,
	fyne.Key4: 0x05, fyne.Key5: 0x06, fyne.Key6: 0x07, fyne.Key7: 0x08,
	fyne.Key8: 0x09, fyne.Key9: 0x0A,

	// Function keys
	fyne.KeyF1: 0x3B, fyne.KeyF2: 0x3C, fyne.KeyF3: 0x3D, fyne.KeyF4: 0x3E,
	fyne.KeyF5: 0x3F, fyne.KeyF6: 0x40, fyne.KeyF7: 0x41, fyne.KeyF8: 0x42,
	fyne.KeyF9: 0x43, fyne.KeyF10: 0x44, fyne.KeyF11: 0x57, fyne.KeyF12: 0x58,

	// Control cluster
	fyne.KeyEscape:    0x01,
	fyne.KeyBackspace: 0x0E,
	fyne.KeyTab:       0x0F,
	fyne.KeyReturn:    0x1C,
	fyne.KeyEnter:     0x1C,
	fyne.KeySpace:     0x39,

	// Punctuation (US layout — matches Windows guest expectations for a
	// physical PC keyboard; layout translation happens in the guest OS).
	fyne.KeyMinus:        0x0C,
	fyne.KeyEqual:        0x0D,
	fyne.KeyLeftBracket:  0x1A,
	fyne.KeyRightBracket: 0x1B,
	fyne.KeyBackslash:    0x2B,
	fyne.KeySemicolon:    0x27,
	fyne.KeyApostrophe:   0x28,
	fyne.KeyBackTick:     0x29,
	fyne.KeyComma:        0x33,
	fyne.KeyPeriod:       0x34,
	fyne.KeySlash:        0x35,
}

// Extended (0xE0-prefixed) scancodes. Arrow keys, right-hand modifiers,
// navigation cluster.
var extScancodes = map[fyne.KeyName]byte{
	fyne.KeyUp:       0x48,
	fyne.KeyDown:     0x50,
	fyne.KeyLeft:     0x4B,
	fyne.KeyRight:    0x4D,
	fyne.KeyHome:     0x47,
	fyne.KeyEnd:      0x4F,
	fyne.KeyPageUp:   0x49,
	fyne.KeyPageDown: 0x51,
	fyne.KeyInsert:   0x52,
	fyne.KeyDelete:   0x53,
}

// runeToScancode maps a printable ASCII rune (US layout) to its PC/AT
// scancode and whether Shift needs to be held. Returns ok=false for runes
// we can't type on a physical US keyboard — the paste-text helper drops
// those with a log line rather than sending garbage.
//
// Handles printable ASCII 0x20-0x7E plus '\n' (Enter) and '\t' (Tab).
// Extended chars (Icelandic Þ/Æ, curly quotes, emoji) are not on a US
// physical keyboard — user should switch to a matching layout in-guest
// or use SPICE clipboard once the agent is installed.
func runeToScancode(r rune) (scancode byte, shift bool, ok bool) {
	switch r {
	case ' ':
		return 0x39, false, true
	case '\n', '\r':
		return 0x1C, false, true
	case '\t':
		return 0x0F, false, true
	case '`':
		return 0x29, false, true
	case '~':
		return 0x29, true, true
	case '-':
		return 0x0C, false, true
	case '_':
		return 0x0C, true, true
	case '=':
		return 0x0D, false, true
	case '+':
		return 0x0D, true, true
	case '[':
		return 0x1A, false, true
	case '{':
		return 0x1A, true, true
	case ']':
		return 0x1B, false, true
	case '}':
		return 0x1B, true, true
	case '\\':
		return 0x2B, false, true
	case '|':
		return 0x2B, true, true
	case ';':
		return 0x27, false, true
	case ':':
		return 0x27, true, true
	case '\'':
		return 0x28, false, true
	case '"':
		return 0x28, true, true
	case ',':
		return 0x33, false, true
	case '<':
		return 0x33, true, true
	case '.':
		return 0x34, false, true
	case '>':
		return 0x34, true, true
	case '/':
		return 0x35, false, true
	case '?':
		return 0x35, true, true
	}

	// Digits and their shifted symbols.
	if r >= '0' && r <= '9' {
		// 0 is scancode 0x0B (last of the row), 1..9 are 0x02..0x0A.
		if r == '0' {
			return 0x0B, false, true
		}
		return byte(r-'1') + 0x02, false, true
	}
	shiftedDigits := ")!@#$%^&*("
	for i, s := range shiftedDigits {
		if r == s {
			if s == ')' {
				return 0x0B, true, true
			}
			return byte(i-1) + 0x02, true, true
		}
	}

	// Letters. Uppercase = shift held.
	if r >= 'a' && r <= 'z' {
		if code, ok := scancodes[fyneKeyForLetter(r)]; ok {
			return code, false, true
		}
	}
	if r >= 'A' && r <= 'Z' {
		if code, ok := scancodes[fyneKeyForLetter(r+0x20)]; ok {
			return code, true, true
		}
	}
	return 0, false, false
}

// fyneKeyForLetter maps a lowercase ASCII letter to the Fyne KeyName
// entry we already have in the scancodes table. Keeps the scancode
// definitions in one place instead of duplicating them per shift-state.
func fyneKeyForLetter(r rune) fyne.KeyName {
	return fyne.KeyName(string(r - 0x20)) // "a" → "A", matches fyne.KeyA
}

// Modifier keys — Fyne fires these through the `desktop` sub-package with
// Left/Right variants. SPICE distinguishes: right control/alt/super are
// extended (0xE0-prefixed), right shift has its own scancode.
var modifierScancodes = map[fyne.KeyName][]byte{
	desktop.KeyShiftLeft:    {0x2A},
	desktop.KeyShiftRight:   {0x36},
	desktop.KeyControlLeft:  {0x1D},
	desktop.KeyControlRight: {0xE0, 0x1D},
	desktop.KeyAltLeft:      {0x38},
	desktop.KeyAltRight:     {0xE0, 0x38},
	desktop.KeySuperLeft:    {0xE0, 0x5B}, // Left Windows / Left Command
	desktop.KeySuperRight:   {0xE0, 0x5C}, // Right Windows / Right Command
}
