package sim

// keyUsage maps a KeyboardEvent.key string to a USB HID usage code and a
// shift flag. Returns ok=false for unsupported keys (e.g. F-keys, IME).
func keyUsage(key string) (usage uint64, shift bool, ok bool) {
	entry, found := keyTable[key]
	if !found {
		return 0, false, false
	}
	return entry.usage, entry.shift, true
}

// keyTable maps KeyboardEvent.key → (USB HID usage code, shift required).
// Usage codes follow the USB HID Usage Tables spec (page 0x07 — Keyboard).
var keyTable map[string]struct {
	usage uint64
	shift bool
}

func init() {
	keyTable = make(map[string]struct {
		usage uint64
		shift bool
	})

	add := func(key string, usage uint64, shift bool) {
		keyTable[key] = struct {
			usage uint64
			shift bool
		}{usage, shift}
	}

	// Lowercase letters: a=4 .. z=29, shift=false
	for i := 0; i < 26; i++ {
		add(string(rune('a'+i)), uint64(4+i), false)
	}
	// Uppercase letters: A=4 .. Z=29, shift=true
	for i := 0; i < 26; i++ {
		add(string(rune('A'+i)), uint64(4+i), true)
	}

	// Digits: 1..9 → 30..38, 0 → 39, shift=false
	for i := 1; i <= 9; i++ {
		add(string(rune('0'+i)), uint64(29+i), false) // '1'→30, '9'→38
	}
	add("0", 39, false)

	// Shifted digit symbols
	add("!", 30, true)
	add("@", 31, true)
	add("#", 32, true)
	add("$", 33, true)
	add("%", 34, true)
	add("^", 35, true)
	add("&", 36, true)
	add("*", 37, true)
	add("(", 38, true)
	add(")", 39, true)

	// Punctuation (shift=false)
	add("-", 45, false)
	add("=", 46, false)
	add("[", 47, false)
	add("]", 48, false)
	add("\\", 49, false)
	add(";", 51, false)
	add("'", 52, false)
	add("`", 53, false)
	add(",", 54, false)
	add(".", 55, false)
	add("/", 56, false)
	add(" ", 44, false)

	// Shifted punctuation (shift=true)
	add("_", 45, true)
	add("+", 46, true)
	add("{", 47, true)
	add("}", 48, true)
	add("|", 49, true)
	add(":", 51, true)
	add("\"", 52, true)
	add("~", 53, true)
	add("<", 54, true)
	add(">", 55, true)
	add("?", 56, true)

	// Named keys (shift=false)
	add("Enter", 40, false)
	add("Escape", 41, false)
	add("Backspace", 42, false)
	add("Tab", 43, false)
	add("Delete", 76, false)
	add("ArrowRight", 79, false)
	add("ArrowLeft", 80, false)
	add("ArrowDown", 81, false)
	add("ArrowUp", 82, false)
}
