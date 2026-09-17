package com.sodre90.cmuxremote.ui.terminal

import androidx.annotation.StringRes
import androidx.compose.ui.input.key.Key
import com.sodre90.cmuxremote.R

// Control sequences built from code points so no raw control bytes live in source.
// ESC is internal (not private) so TerminalScreen's physical-keyboard handling
// can send it directly, the same sequence the on-screen "Esc" key uses.
internal val ESC = Char(27).toString()

// ^C/^D/^Z are derived from [ctrlSequence] (not their own Char literals) so the
// quick buttons and the general Ctrl chip are guaranteed byte-identical for the
// combos they overlap on.
private val CTRL_C = ctrlSequence('C')!!
private val CTRL_D = ctrlSequence('D')!!
private val CTRL_Z = ctrlSequence('Z')!!
private const val CR = "\r"

/**
 * A key in the terminal key bar. Most keys emit a fixed byte string; the cursor
 * keys (arrows, Home, End) instead resolve their escape against DECCKM — the
 * terminal's "application cursor keys" mode — which is only known at press time.
 */
sealed interface TerminalKey {
    val label: String

    /** What TalkBack announces for this key's button -- describes the action
     *  (e.g. "Send Ctrl+C (interrupt)"), not [label]'s terminal-glyph shorthand
     *  (e.g. "^C"), which reads as meaningless punctuation to a screen reader.
     *  A resource id (not a resolved String) since this list is built outside
     *  any @Composable context -- resolved via stringResource() at the button's
     *  call site instead (see ArrowButton/KeyBar in TerminalScreen). */
    @get:StringRes
    val contentDescriptionRes: Int
    fun sequence(applicationCursorKeys: Boolean): String
}

/** A key whose byte sequence never changes. */
data class StaticKey(
    override val label: String,
    private val bytes: String,
    @StringRes override val contentDescriptionRes: Int,
) : TerminalKey {
    override fun sequence(applicationCursorKeys: Boolean): String = bytes
}

/**
 * A cursor key. When DECCKM is set the terminal expects SS3 (`ESC O x`); when
 * reset it expects CSI (`ESC [ x`). [finalChar] is the trailing letter (A/B/C/D
 * for the arrows, H/F for Home/End) shared by both forms.
 */
data class CursorKey(
    override val label: String,
    private val finalChar: Char,
    @StringRes override val contentDescriptionRes: Int,
) : TerminalKey {
    override fun sequence(applicationCursorKeys: Boolean): String =
        ESC + (if (applicationCursorKeys) "O" else "[") + finalChar
}

// The directional arrows, surfaced as an always-visible D-pad (see ArrowPad in
// TerminalScreen) rather than buried in the scrollable bar. DECCKM-aware like any
// CursorKey. The final chars D/C (not C/D) match the ANSI codes for left/right.
val ArrowUp = CursorKey("↑", 'A', contentDescriptionRes = R.string.key_desc_up)
val ArrowDown = CursorKey("↓", 'B', contentDescriptionRes = R.string.key_desc_down)
val ArrowLeft = CursorKey("←", 'D', contentDescriptionRes = R.string.key_desc_left)
val ArrowRight = CursorKey("→", 'C', contentDescriptionRes = R.string.key_desc_right)

/** The horizontally-scrolling key bar. The arrows live in the D-pad instead. */
val TerminalKeys: List<TerminalKey> = listOf(
    StaticKey("Esc", ESC, contentDescriptionRes = R.string.key_desc_escape),
    StaticKey("Tab", "\t", contentDescriptionRes = R.string.key_desc_tab),
    StaticKey("⏎", CR, contentDescriptionRes = R.string.key_desc_enter),
    StaticKey("^C", CTRL_C, contentDescriptionRes = R.string.key_desc_ctrl_c),
    StaticKey("^D", CTRL_D, contentDescriptionRes = R.string.key_desc_ctrl_d),
    StaticKey("^Z", CTRL_Z, contentDescriptionRes = R.string.key_desc_ctrl_z),
    CursorKey("Home", 'H', contentDescriptionRes = R.string.key_desc_home),
    CursorKey("End", 'F', contentDescriptionRes = R.string.key_desc_end),
    // Page keys use the `~` (keypad) form and are unaffected by DECCKM.
    StaticKey("PgUp", ESC + "[5~", contentDescriptionRes = R.string.key_desc_page_up),
    StaticKey("PgDn", ESC + "[6~", contentDescriptionRes = R.string.key_desc_page_down),
)

/**
 * Resolves a physical keyboard press (e.g. from a Bluetooth keyboard) to the
 * terminal byte sequence it should send, or null if this key isn't one of the
 * ones intercepted directly -- regular character keys fall through to the
 * text field's own IME-driven capture instead.
 */
internal fun physicalKeySequence(key: Key, applicationCursorKeys: Boolean): String? = when (key) {
    Key.DirectionUp -> ArrowUp.sequence(applicationCursorKeys)
    Key.DirectionDown -> ArrowDown.sequence(applicationCursorKeys)
    Key.DirectionLeft -> ArrowLeft.sequence(applicationCursorKeys)
    Key.DirectionRight -> ArrowRight.sequence(applicationCursorKeys)
    Key.Escape -> ESC
    Key.Tab -> "\t"
    Key.Enter, Key.NumPadEnter -> CR
    else -> null
}

/**
 * Maps a single Latin letter to its Ctrl+<letter> control byte -- Ctrl+A is
 * 0x01 through Ctrl+Z's 0x1A, the ASCII convention every terminal expects for
 * readline/vim/tmux bindings (Ctrl+L, Ctrl+A/E, Ctrl+R, ...). Case-insensitive,
 * since a physical Shift key doesn't change what Ctrl+<letter> sends. Returns
 * null for anything that isn't a single Latin letter -- there's no
 * conventional Ctrl byte for those here.
 */
internal fun ctrlSequence(letter: Char): String? {
    val upper = letter.uppercaseChar()
    if (upper !in 'A'..'Z') return null
    return Char(upper - 'A' + 1).toString()
}

/**
 * The terminal key bar's latching Ctrl chip, applied to whatever [text] was
 * about to be sent: rewritten to its Ctrl combination when it's exactly one
 * letter, otherwise returned unchanged. Multi-character text (paste, IME
 * composition, an already-built key-bar escape sequence) and non-letter
 * single characters have no Ctrl+<key> byte in this mapping, so they pass
 * through as typed -- only the chip's arming state (owned by the caller)
 * decides whether this runs at all.
 */
internal fun applyCtrlArm(text: String): String = text.singleOrNull()?.let { ctrlSequence(it) } ?: text
