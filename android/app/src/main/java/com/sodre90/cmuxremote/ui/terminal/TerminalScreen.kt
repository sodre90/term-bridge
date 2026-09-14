package com.sodre90.cmuxremote.ui.terminal

import android.content.res.Configuration
import android.net.Uri
import android.util.Log
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.ScrollState
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.gestures.calculateZoom
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.WindowInsetsSides
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.ime
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.only
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.union
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.drawWithContent
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.BlendMode
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.CompositingStrategy
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.input.nestedscroll.NestedScrollConnection
import androidx.compose.ui.input.nestedscroll.NestedScrollSource
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.input.pointer.PointerEventPass
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.input.pointer.positionChangeIgnoreConsumed
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.LocalSoftwareKeyboardController
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.tooling.preview.Preview
import androidx.compose.ui.unit.Velocity
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.sodre90.cmuxremote.BuildConfig
import com.sodre90.cmuxremote.R
import com.sodre90.cmuxremote.model.DecodedGrid
import com.sodre90.cmuxremote.ui.LocalHostName
import com.sodre90.cmuxremote.ui.UiState
import com.sodre90.cmuxremote.ui.YoloBadge
import com.sodre90.cmuxremote.ui.layout.PlacementSheet
import com.sodre90.cmuxremote.ui.sessions.ClosePaneDialog
import com.sodre90.cmuxremote.ui.sessions.actionOutcomeText
import com.sodre90.cmuxremote.ui.theme.CmuxTheme
import com.sodre90.cmuxremote.ui.yoloModeLabel
import kotlinx.coroutines.delay
import kotlin.math.abs

private const val TAG = "TerminalSwipe"

/** Finger travel that buys one wheel notch, on panes scrolled that way. A notch
 *  moves such a pane well under a row, so this is deliberately short. */
private val WHEEL_NOTCH_TRAVEL = 10.dp

/**
 * Smallest gap between wheel notches.
 *
 * Not a feel preference -- a transport limit. Each input RPC is a bridge
 * subprocess spawn (~150ms; see [DeliveryTracker]), and anything emitted while
 * one is in flight coalesces into the next write. Notches are worthless once
 * stale, so surplus is DROPPED rather than queued: without this a fast flick
 * builds a blob of dozens that lands as one write long after lift-off, and a
 * blob that size is collapsed by the pane into almost no movement at all
 * (measured: 0 to 34 rows for the same payload). Spacing them keeps each write
 * small enough to survive (cmux-app-vcx).
 */
private const val WHEEL_NOTCH_INTERVAL_MS = 40L

// Reference size for the surface-viewport resize math (decoupled from the display
// zoom so pinching never re-resizes the surface).
private const val BASE_FONT_SP = 13f

// Display font bounds. The fit-to-width baseline lives in [MIN_FONT_SP, FIT_MAX_SP];
// pinching in can grow it up to MAX_FONT_SP.
private const val MIN_FONT_SP = 7f
private const val FIT_MAX_SP = 22f
private const val MAX_FONT_SP = 28f

// Pinch range, multiplied onto the fit baseline: 1x = exact fit, up to 6x to read in.
// Not private: also the bounds/step for the font-size stepper on
// ConnectionSettingsScreen, which edits the same persisted zoom value.
const val MIN_ZOOM = 1f
const val MAX_ZOOM = 6f
const val ZOOM_STEP = 0.25f

private val LandscapeTopBarHeight = 40.dp

/** Longer than this, a single-line paste is worth confirming too: it is well
 *  past anything you would type by hand into a prompt. */
private const val PASTE_CONFIRM_CHARS = 200

/** Enough of the clipboard to judge it by without building a whole viewer. */
private const val PASTE_PREVIEW_CHARS = 2000

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun TerminalScreen(
    vm: TerminalViewModel,
    onBack: () -> Unit,
    onOpenSurface: (String) -> Unit = {},
) {
    val state by vm.state.collectAsState()
    val yoloMode by vm.yoloMode.collectAsState()
    val paneLabel by vm.paneLabel.collectAsState()
    val workspaceId by vm.workspaceId.collectAsState()
    val placement by vm.placement.state.collectAsState()
    val actionOutcome by vm.actionOutcome.collectAsState()
    var closingPane by rememberSaveable { mutableStateOf(false) }
    val snackbar = remember { SnackbarHostState() }
    actionOutcome?.let { outcome ->
        val text = actionOutcomeText(outcome)
        LaunchedEffect(outcome) {
            snackbar.showSnackbar(text)
            vm.dismissActionOutcome()
        }
    }
    val landscape = LocalConfiguration.current.orientation == Configuration.ORIENTATION_LANDSCAPE
    val deliveryStatus by vm.deliveryStatus.collectAsState()
    val lostInputNotice by vm.lostInputNotice.collectAsState()
    val attachOutcome by vm.attachOutcome.collectAsState()
    val attachmentDraft by vm.attachmentDraft.collectAsState()
    val pickImage = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri ->
        uri?.let(vm::stageAttachment)
    }
    // Not rendered anywhere -- kept only so diffToKeystrokes has an old value
    // to diff each keystroke against. The invisible capture field below is the
    // only place typed input touches the UI; the terminal's own echo is the
    // single visible record of what's been typed, instead of mirroring it in
    // a second, separately-scrolling box.
    //
    // Deliberately remember, not rememberSaveable: this mirrors the REMOTE
    // line, which is not saved alongside it, so a value restored into a fresh
    // composition describes a line that no longer exists. The first keystroke
    // then diffs against that ghost and retypes the whole of it onto the live
    // terminal (cmux-app-0jh). Losing the base instead costs only the erase
    // half of the diff -- typing appends, and backspace on an empty field
    // already sends DEL directly below. It also keeps typed input, which can
    // be a password, from leaving the process in saved instance state.
    var input by remember { mutableStateOf("") }
    val clipboard = LocalClipboardManager.current
    val focusRequester = remember { FocusRequester() }
    val keyboardController = LocalSoftwareKeyboardController.current

    // The "lost input" notice is a one-shot signal (see TerminalViewModel) --
    // show it briefly, then tell the view model it's been seen.
    LaunchedEffect(lostInputNotice) {
        if (lostInputNotice) {
            delay(4_000)
            vm.dismissLostInputNotice()
        }
    }
    LaunchedEffect(attachOutcome) {
        if (attachOutcome != null) {
            delay(4_000)
            vm.dismissAttachOutcome()
        }
    }

    // Remote terminal sessions are watched, not typed into continuously - don't
    // let the screen sleep mid-session. Reset on leaving so the rest of the app
    // keeps normal screen-timeout behavior.
    val view = LocalView.current
    DisposableEffect(Unit) {
        view.keepScreenOn = true
        onDispose { view.keepScreenOn = false }
    }

    // Pinch-to-zoom factor over the fit-to-width baseline (1f = exact fit).
    // Seeded from the persisted preference (see TerminalDisplayStore) so a
    // size set here or on ConnectionSettingsScreen survives leaving and
    // reopening a terminal, or restarting the app.
    var userZoom by rememberSaveable { mutableFloatStateOf(vm.loadFontZoom()) }
    // Read once per screen: the preference is edited on the settings screen, so
    // it cannot change while a terminal is on top of it.
    val wheelScrolls = remember { vm.loadWheelScrolling() }
    // Word-wrap: on → zooming in reflows long rows onto extra lines; off → it stays
    // one row per line with horizontal panning (keeps tables/TUI layouts aligned).
    var wrap by rememberSaveable { mutableStateOf(true) }

    // Ctrl chip: armed by one tap, consumed by the next key sent (through
    // [sendKey], the single funnel every key-bar button, typed-letter diff,
    // and physical-key send below goes through) -- then disarms. Letters get
    // rewritten to their Ctrl byte via applyCtrlArm; anything else the chip
    // can't map (arrows, paste, PgUp, ...) is sent unchanged, but still
    // consumes the arm, since the user's next key press is the one it applies
    // to regardless of whether that key had a Ctrl form.
    var ctrlArmed by rememberSaveable { mutableStateOf(false) }

    // Clipboard content held back for confirmation -- see [needsPasteConfirmation].
    var pendingPaste by rememberSaveable { mutableStateOf<String?>(null) }

    val sendKey: (String) -> Unit = { text ->
        if (ctrlArmed) {
            vm.sendText(applyCtrlArm(text))
            ctrlArmed = false
        } else {
            vm.sendText(text)
        }
    }

    // Paste goes through sendKey like everything else, so the Ctrl chip still
    // disarms on it, but is bracketed first when the pane asked for that.
    val sendPaste: (String) -> Unit = { text ->
        sendKey(bracketPaste(text, (state as? UiState.Ready)?.data?.grid?.bracketedPaste == true))
    }

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                title = {
                    Row(
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        // The workspace's color, same as its dot on the sessions
                        // list, so the identity carries across the navigation.
                        parseColor(paneLabel.color)?.let { accent ->
                            Surface(
                                color = accent,
                                shape = CircleShape,
                                border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
                                modifier = Modifier.size(10.dp),
                            ) {}
                        }
                        Column(modifier = Modifier.weight(1f, fill = false)) {
                            Text(
                                text = paneLabel.workspace.ifBlank { stringResource(R.string.terminal_title) },
                                style = if (landscape) {
                                    MaterialTheme.typography.titleMedium
                                } else {
                                    MaterialTheme.typography.titleLarge
                                },
                                maxLines = 1,
                                overflow = TextOverflow.Ellipsis,
                            )
                            if (!landscape && paneLabel.pane.isNotBlank()) {
                                Text(
                                    text = paneLabel.pane,
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 1,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                        yoloModeLabel(yoloMode)?.let { YoloBadge(it) }
                    }
                },
                navigationIcon = {
                    TextButton(onClick = onBack) { Text(stringResource(R.string.action_back)) }
                },
                actions = {
                    TextButton(onClick = { vm.reconnect() }) { Text(stringResource(R.string.action_refresh)) }
                    TextButton(onClick = { wrap = !wrap }) {
                        Text(stringResource(if (wrap) R.string.terminal_wrap_on else R.string.terminal_wrap_off))
                    }
                    PaneActionsMenu(
                        enabled = workspaceId != null,
                        tabs = vm.hostHasTabs(),
                        onSplit = { vm.openPlacement() },
                        onNewTab = { vm.newTab(onCreated = onOpenSurface) },
                        onShowOnMac = { vm.showOnMac() },
                        onClose = { closingPane = true },
                    )
                },
                // Landscape on a phone leaves ~360dp of height; the stock 64dp bar
                // plus the status bar took a third of it, and the rows it cost are
                // the whole reason to rotate. The pane subtitle drops with it.
                expandedHeight = if (landscape) {
                    LandscapeTopBarHeight
                } else {
                    TopAppBarDefaults.TopAppBarExpandedHeight
                },
            )
        },
        bottomBar = {
            // A custom bottomBar (plain Column) does not consume insets the way
            // NavigationBar/BottomAppBar do, so apply them here: lift the bar above
            // the system navigation bar, and above the IME when it opens. Union (not
            // chained padding) so the two bottom insets don't stack. Horizontal is
            // included because a landscape 3-button nav bar sits on one *side*, and
            // dropping that inset put Paste and PgDn underneath it -- misaligned with
            // the grid above, which respects the side inset already.
            Column(
                modifier = Modifier.windowInsetsPadding(
                    WindowInsets.navigationBars.union(WindowInsets.ime)
                        .only(WindowInsetsSides.Bottom + WindowInsetsSides.Horizontal),
                ),
            ) {
                val appCursorKeys = (state as? UiState.Ready)?.data?.grid?.applicationCursorKeys ?: false
                ArrowPad(
                    applicationCursorKeys = appCursorKeys,
                    onKey = sendKey,
                    onPaste = {
                        clipboard.getText()?.text?.let { text ->
                            if (needsPasteConfirmation(text)) pendingPaste = text else sendPaste(text)
                        }
                    },
                    onAttachFromGallery = {
                        pickImage.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly))
                    },
                    onAttachFromClipboard = vm::stageAttachment,
                )
                KeyBar(
                    applicationCursorKeys = appCursorKeys,
                    ctrlArmed = ctrlArmed,
                    onToggleCtrl = { ctrlArmed = !ctrlArmed },
                    onKey = sendKey,
                )
                DeliveryStatusLabel(
                    status = deliveryStatus,
                    lostInputNotice = lostInputNotice,
                    attachOutcome = attachOutcome,
                )
            }
        },
    ) { inner ->
        Box(modifier = Modifier.fillMaxSize().padding(inner)) {
            when (val s = state) {
                is UiState.Error -> Column(
                    modifier = Modifier.align(Alignment.Center).padding(24.dp),
                    horizontalAlignment = Alignment.CenterHorizontally,
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Text(s.message)
                    Button(onClick = { vm.reconnect() }) { Text(stringResource(R.string.action_reconnect)) }
                }

                is UiState.Loading -> CircularProgressIndicator(Modifier.align(Alignment.Center))

                is UiState.Ready -> {
                    val grid = s.data.grid
                    val styles = s.data.styles
                    val measurer = rememberTextMeasurer()
                    val density = LocalDensity.current
                    // Glyph advance per 1sp, measured once at the base size. The font is
                    // scalable, so advance scales linearly with size — this lets us solve
                    // for the font that makes the surface width fit the viewport.
                    val advancePerSp = remember {
                        val w = measurer.measure(
                            AnnotatedString("MMMMMMMMMM"),
                            style = TextStyle(fontFamily = TerminalFont, fontSize = BASE_FONT_SP.sp),
                        ).size.width / 10f
                        w / BASE_FONT_SP
                    }
                    // Surface-viewport (resize) cell box: a fixed reference, independent of
                    // display zoom, so pinching never re-resizes the surface.
                    val cellWBase = advancePerSp * BASE_FONT_SP
                    val cellHBase = with(density) { (BASE_FONT_SP * TerminalLineHeightFactor).sp.toPx() }
                    BoxWithConstraints(
                        modifier = Modifier
                            .fillMaxSize()
                            .pointerInput(Unit) {
                                // Reliable pinch: claim multi-touch on the Initial pass so the
                                // grid's vertical/horizontal scroll never fights it. Single-finger
                                // events are left unconsumed and fall through to those scrolls.
                                awaitEachGesture {
                                    awaitFirstDown(
                                        requireUnconsumed = false,
                                        pass = PointerEventPass.Initial,
                                    )
                                    var zoomChanged = false
                                    do {
                                        val event = awaitPointerEvent(PointerEventPass.Initial)
                                        if (event.changes.count { it.pressed } >= 2) {
                                            val zoom = event.calculateZoom()
                                            if (zoom != 1f) {
                                                userZoom = (userZoom * zoom).coerceIn(MIN_ZOOM, MAX_ZOOM)
                                                zoomChanged = true
                                                event.changes.forEach { if (it.pressed) it.consume() }
                                            }
                                        }
                                    } while (event.changes.any { it.pressed })
                                    // Persist once per completed pinch gesture rather than on
                                    // every intermediate frame -- a pinch can fire dozens of
                                    // zoom deltas a second, and a plain tap (the common case,
                                    // used to focus the keyboard) never touches zoom at all.
                                    if (zoomChanged) vm.saveFontZoom(userZoom)
                                }
                            }
                            // Tapping the terminal is how you start typing -- there's no
                            // separate input box to tap into anymore.
                            .pointerInput(Unit) {
                                detectTapGestures {
                                    focusRequester.requestFocus()
                                    keyboardController?.show()
                                }
                            }
                    ) {
                        // RenderGridView insets its content by 8.dp on every side; subtract
                        // it so the measured fit matches the real text area (no clipped edge).
                        val padPx = with(density) { 8.dp.toPx() }
                        val wPx = with(density) { maxWidth.toPx() } - 2 * padPx
                        val hPx = with(density) { maxHeight.toPx() } - 2 * padPx
                        val (cols, rows) = gridDimensions(wPx, hPx, cellWBase, cellHBase)
                        // resize() only fires when (cols,rows) actually change.
                        LaunchedEffect(cols, rows) { vm.resize(cols, rows) }

                        // Fit-to-width: size the font so the surface's full column count fits
                        // the viewport; pinch (userZoom) grows it from there to read a section.
                        val gridCols = grid.columns.takeIf { it > 0 } ?: cols
                        val fitFontSp = fitFontSizeSp(wPx, gridCols, advancePerSp, MIN_FONT_SP, FIT_MAX_SP)
                        val fontSizeSp = (fitFontSp * userZoom).coerceIn(MIN_FONT_SP, MAX_FONT_SP)

                        val paneOverscrollPager = rememberPaneOverscrollPager(
                            grid = grid,
                            wheelScrolls = wheelScrolls,
                            viewportHeightPx = hPx,
                            onSend = vm::sendText,
                        )

                        RenderGridView(
                            grid = grid,
                            styles = styles,
                            fontSizeSp = fontSizeSp,
                            wrap = wrap,
                            modifier = Modifier
                                .fillMaxSize()
                                .padding(8.dp)
                                .nestedScroll(paneOverscrollPager),
                        )

                        // The real capture point for the keyboard: fully transparent and
                        // 1dp so nothing renders, but still focusable, so the terminal's
                        // own echo (via RenderGridView above) is the only place typed text
                        // is visible -- not duplicated in a second on-screen field.
                        BasicTextField(
                            value = input,
                            onValueChange = { new ->
                                val diff = diffToKeystrokes(input, new)
                                if (BuildConfig.DEBUG) {
                                    Log.d(
                                        "TerminalInput",
                                        "onValueChange old=${describeForLog(input)} " +
                                            "new=${describeForLog(new)} diff=${describeForLog(diff)}",
                                    )
                                }
                                if (diff.isNotEmpty()) sendKey(diff)
                                input = new
                            },
                            textStyle = TextStyle(color = Color.Transparent),
                            cursorBrush = SolidColor(Color.Transparent),
                            singleLine = true,
                            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Send),
                            keyboardActions = KeyboardActions(onSend = {
                                sendKey("\r")
                                input = ""
                            }),
                            modifier = Modifier
                                .size(1.dp)
                                .alpha(0f)
                                .focusRequester(focusRequester)
                                // Backspace on an already-empty field is a no-op as far as
                                // the field's own text is concerned, so onValueChange never
                                // fires -- there's nothing for diffToKeystrokes to diff. Most
                                // IMEs (Gboard included) still dispatch a raw KEYCODE_DEL in
                                // that case for compatibility; catch it here and send the
                                // erase byte directly.
                                //
                                // Keys with no sensible meaning inside a single-line text
                                // field (arrows, Escape, Tab, Enter -- from a physical/
                                // Bluetooth keyboard) are intercepted the same way, instead
                                // of falling through into the field's own IME-driven capture
                                // where they'd be dropped or mangled.
                                .onPreviewKeyEvent { event ->
                                    if (event.type != KeyEventType.KeyDown) {
                                        return@onPreviewKeyEvent false
                                    }
                                    if (event.key == Key.Backspace && input.isEmpty()) {
                                        sendKey(DEL)
                                        return@onPreviewKeyEvent true
                                    }
                                    val sequence = physicalKeySequence(event.key, grid.applicationCursorKeys)
                                        ?: return@onPreviewKeyEvent false
                                    sendKey(sequence)
                                    if (event.key == Key.Enter || event.key == Key.NumPadEnter) input = ""
                                    true
                                },
                        )
                    }
                }
            }
            // Overlaid rather than laid out above the grid on purpose: the grid's
            // measured height is what the resize RPC reports to the Mac, so a
            // banner taking part in the layout would resize the real terminal
            // every time the socket blipped.
            if ((state as? UiState.Ready)?.data?.stale == true) {
                StaleScreenBanner(modifier = Modifier.align(Alignment.TopCenter))
            }
        }
    }

    pendingPaste?.let { text ->
        PasteConfirmationDialog(
            text = text,
            onConfirm = {
                pendingPaste = null
                sendPaste(text)
            },
            onDismiss = { pendingPaste = null },
        )
    }
    if (closingPane) {
        ClosePaneDialog(
            paneName = paneLabel.pane,
            onConfirm = {
                closingPane = false
                vm.closePane(onClosed = onBack)
            },
            onDismiss = { closingPane = false },
        )
    }

    placement?.let { state ->
        PlacementSheet(
            state = state,
            initialSurfaceId = vm.surfaceId,
            onCreate = { target, where -> vm.placement.createPane(target, where, onCreated = onOpenSurface) },
            onDismiss = { vm.placement.close() },
        )
    }

    attachmentDraft?.let { draft ->
        AttachmentDialog(
            draft = draft,
            onSend = vm::sendAttachment,
            onDismiss = vm::discardAttachment,
        )
    }
}

/**
 * Hands the pane whatever vertical drag the local render buffer could not
 * absorb, as page keys (or wheel notches; see [wheelScrolls]).
 *
 * A handoff, NOT an interception. The grid's own verticalScroll gets first
 * refusal on every gesture and only its overscroll -- `available` in
 * [NestedScrollConnection.onPostScroll] -- reaches the pager, so panning the
 * visible grid always works and paging the pane begins where panning runs out.
 * An earlier version claimed the whole drag on the Initial pass whenever
 * [DecodedGrid.mayPageOnOverscroll] was true, which made every alt-screen pane
 * unpannable -- 79 rows against a phone viewport, so the top half was simply
 * unreachable -- and sent page keys to panes that pay no attention to them, an
 * idle shell stranded on the alternate screen among them (cmux-app-4yi).
 *
 * Nested scroll is what makes this expressible at all: Compose's pointer
 * consumption is all-or-nothing, so a descendant scroll that consumed a drag
 * looks identical to one that hit its edge. Only the nested-scroll cycle
 * reports how much was actually used.
 *
 * The keys are PgUp/PgDn, NOT synthetic wheel events. Verified twice: opencode
 * prints SGR/X10 sequences as literal text (d3da2ba), and a Claude pane
 * advertising mouse tracking WITH SGR (1000+1002+1003+1006, on the alternate
 * screen) silently swallows `ESC[<64;col;rowM` -- the pane stopped scrolling
 * entirely until this was put back. Whatever makes that pane scroll under a
 * desktop trackpad, it is not a wheel report arriving on stdin (cmux-app-vcx).
 */
@Composable
internal fun rememberPaneOverscrollPager(
    grid: DecodedGrid,
    wheelScrolls: Boolean,
    viewportHeightPx: Float,
    onSend: (String) -> Unit,
): NestedScrollConnection {
    // Two ways to move a pane, and the pane plus a preference decide which.
    //
    // Wheel notches move it a fraction of a row, so it tracks the finger. Their
    // cost is round trips: a half-screen is roughly sixty notches against a
    // ~150ms-per-RPC bridge, so this is smooth-but-slow, and throttled so a
    // flick cannot build a blob the pane would collapse (WHEEL_NOTCH_INTERVAL_MS).
    //
    // PgUp/PgDn covers that distance in one keystroke, but its quantum is fixed
    // at half a screen (35 of 79 rows on a live Claude pane). Soft-wrapped, 35
    // rows are taller than the viewport, so 1:1 is unreachable and a step only
    // decides how much overscroll buys one jump -- sized to one comfortable
    // swipe. Half the viewport was longer than a thumb reaches, so no step ever
    // fired (cmux-app-sgy).
    val wheeling = wheelScrolls && grid.scrollsByWheel
    val notchTravelPx = with(LocalDensity.current) { WHEEL_NOTCH_TRAVEL.toPx() }
    val hoverColumn = grid.columns / 2 + 1
    val hoverRow = grid.rows / 2 + 1
    val pager = remember(grid.mayPageOnOverscroll, wheeling, viewportHeightPx, hoverColumn, hoverRow) {
        if (!grid.mayPageOnOverscroll) return@remember null
        val stepPx = (if (wheeling) notchTravelPx else viewportHeightPx / 4f).coerceAtLeast(1f)
        SwipePager(
            pageStepPx = stepPx,
            // Nothing moves until the first step fires, and it then costs two
            // `cmux rpc` subprocess spawns (~300ms) to become visible, so a
            // quarter-screen of overscroll before any feedback read as lag.
            // Half that to open with; the steadier spacing resumes after.
            firstStepPx = (if (wheeling) stepPx else stepPx / 2f).coerceAtLeast(1f),
            minStepIntervalMs = if (wheeling) WHEEL_NOTCH_INTERVAL_MS else 0L,
            onStep = { up ->
                if (BuildConfig.DEBUG) Log.d(TAG, "step up=$up wheel=$wheeling")
                // Sent as text so the Ctrl chip's arming is not involved.
                onSend(
                    if (wheeling) wheelNotch(up, hoverColumn, hoverRow) else if (up) "$ESC[5~" else "$ESC[6~",
                )
            },
        )
    }
    return remember(pager) {
        object : NestedScrollConnection {
            override fun onPostScroll(
                consumed: Offset,
                available: Offset,
                source: NestedScrollSource,
            ): Offset {
                // UserInput only: a fling's leftover would page the pane long
                // after the finger is gone, and a programmatic stick-to-bottom
                // scroll is not a scroll the user asked to continue.
                if (pager != null && source == NestedScrollSource.UserInput) {
                    pager.onOverscroll(available.x, available.y)
                }
                // Nothing consumed, so the overscroll stretch still shows the
                // grid has run out -- the only feedback available during the
                // round trip the pane's own jump costs.
                return Offset.Zero
            }

            override suspend fun onPreFling(available: Velocity): Velocity {
                pager?.reset()
                return Velocity.Zero
            }
        }
    }
}

/**
 * Whether [text] is enough of a commitment to ask about before it reaches the
 * shell. Paste used to send the clipboard straight through, so a multi-line
 * copy ran line by line as commands with nothing shown first -- and the phone's
 * clipboard is rarely full of things you meant to execute.
 *
 * A single short line is left alone: that is the ordinary case (a path, a
 * branch name, a token) and a dialog on every one of them would be worse than
 * the risk it guards.
 */
internal fun needsPasteConfirmation(text: String): Boolean =
    '\n' in text.trimEnd('\n') || text.length > PASTE_CONFIRM_CHARS

/** A line count for the dialog: trailing newlines are the copy's terminator,
 *  not empty lines the user needs warning about. */
/**
 * Wraps [text] in the bracketed-paste markers when the pane has DEC private
 * mode 2004 on, so the receiving application takes it as one paste rather than
 * as typing -- without which a multi-line paste runs a line at a time as it
 * lands (cmux-app-ybb).
 *
 * The end marker is stripped from the payload first. Text carrying its own
 * ESC[201~ would otherwise close the bracket early and let whatever followed
 * arrive as ordinary typed input -- the point of bracketing is that the
 * receiver decides what to do with the block, and that guarantee cannot be
 * left to the contents of someone's clipboard.
 */
internal fun bracketPaste(text: String, enabled: Boolean): String =
    if (enabled) BRACKETED_PASTE_START + text.replace(BRACKETED_PASTE_END, "") + BRACKETED_PASTE_END else text

private val BRACKETED_PASTE_START = ESC + "[200~"
private val BRACKETED_PASTE_END = ESC + "[201~"

internal fun pasteLineCount(text: String): Int = text.trimEnd('\n').count { it == '\n' } + 1

@Composable
private fun PasteConfirmationDialog(text: String, onConfirm: () -> Unit, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(stringResource(R.string.terminal_paste_dialog_title)) },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                val lines = pasteLineCount(text)
                Text(pluralStringResource(R.plurals.terminal_paste_dialog_lines, lines, lines))
                // The clipboard itself, so the decision is made on what will
                // actually be sent rather than on a line count alone.
                Text(
                    text = text.take(PASTE_PREVIEW_CHARS),
                    style = MaterialTheme.typography.bodySmall,
                    fontFamily = FontFamily.Monospace,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.heightIn(max = 200.dp).verticalScroll(rememberScrollState()),
                )
            }
        },
        confirmButton = {
            TextButton(onClick = onConfirm) { Text(stringResource(R.string.terminal_paste)) }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text(stringResource(R.string.action_cancel)) }
        },
    )
}

/**
 * Says the grid is the last known screen, not the live one. The terminal
 * deliberately keeps the old frame through a reconnect instead of showing an
 * error page -- which leaves a frozen frame looking exactly like an idle agent
 * unless something says otherwise.
 */
@Composable
private fun StaleScreenBanner(modifier: Modifier = Modifier) {
    Row(
        modifier = modifier
            .padding(8.dp)
            .background(MaterialTheme.colorScheme.errorContainer, RoundedCornerShape(6.dp))
            .padding(horizontal = 10.dp, vertical = 5.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        CircularProgressIndicator(
            modifier = Modifier.size(10.dp),
            strokeWidth = 2.dp,
            color = MaterialTheme.colorScheme.onErrorContainer,
        )
        Text(
            text = stringResource(R.string.terminal_reconnecting_stale),
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onErrorContainer,
        )
    }
}

/**
 * A small, easy-to-miss-on-purpose line reporting delivery trouble: recent
 * input that's stuck unconfirmed, or a reconnect that dropped some in-flight
 * input whose fate is now unknowable. Renders nothing when everything's
 * confirmed, so normal typing never shows a persistent status line.
 */
@Composable
private fun DeliveryStatusLabel(
    status: DeliveryStatus,
    lostInputNotice: Boolean,
    attachOutcome: AttachOutcome? = null,
) {
    val textRes = when {
        attachOutcome != null -> attachOutcomeTextRes(attachOutcome)
        lostInputNotice -> R.string.terminal_delivery_reconnected
        status == DeliveryStatus.DELAYED -> R.string.terminal_delivery_delayed
        status == DeliveryStatus.SENDING -> R.string.status_sending
        else -> null
    }
    val text = textRes?.let { stringResource(it) }
    if (text != null) {
        Text(
            text,
            modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 2.dp),
            style = MaterialTheme.typography.labelSmall,
        )
    }
}

@Preview(showBackground = true)
@Composable
private fun DeliveryStatusLabelSendingPreview() {
    CmuxTheme {
        DeliveryStatusLabel(status = DeliveryStatus.SENDING, lostInputNotice = false)
    }
}

@Preview(showBackground = true, name = "Delayed")
@Composable
private fun DeliveryStatusLabelDelayedPreview() {
    CmuxTheme {
        DeliveryStatusLabel(status = DeliveryStatus.DELAYED, lostInputNotice = false)
    }
}

@Preview(showBackground = true, name = "Lost input notice")
@Composable
private fun DeliveryStatusLabelLostInputPreview() {
    CmuxTheme {
        DeliveryStatusLabel(status = DeliveryStatus.CONFIRMED, lostInputNotice = true)
    }
}

/**
 * The pane's own actions, behind one icon: the bar has no room for more
 * words next to Refresh and Wrap. Disabled until the owning workspace is
 * known, since every route is keyed by it. Splitting joins this menu once
 * the placement preview exists; the tab entry only exists on a host that
 * has tabs.
 */
@Composable
private fun PaneActionsMenu(
    enabled: Boolean,
    tabs: Boolean,
    onSplit: () -> Unit,
    onNewTab: () -> Unit,
    onShowOnMac: () -> Unit,
    onClose: () -> Unit,
) {
    var open by remember { mutableStateOf(false) }
    Box {
        IconButton(onClick = { open = true }, enabled = enabled) {
            Icon(Icons.Default.MoreVert, contentDescription = stringResource(R.string.terminal_pane_actions))
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            DropdownMenuItem(
                text = { Text(stringResource(R.string.terminal_split)) },
                onClick = {
                    open = false
                    onSplit()
                },
            )
            if (tabs) {
                DropdownMenuItem(
                    text = { Text(stringResource(R.string.terminal_new_tab)) },
                    onClick = {
                        open = false
                        onNewTab()
                    },
                )
            }
            DropdownMenuItem(
                text = { Text(stringResource(R.string.terminal_show_on_mac, LocalHostName.current)) },
                onClick = {
                    open = false
                    onShowOnMac()
                },
            )
            HorizontalDivider()
            DropdownMenuItem(
                text = { Text(stringResource(R.string.terminal_close_pane), color = MaterialTheme.colorScheme.error) },
                onClick = {
                    open = false
                    onClose()
                },
            )
        }
    }
}

/**
 * An always-visible single row of arrow keys above the scrollable key bar, so menu
 * navigation never requires scrolling to find them. Each arrow resolves SS3 vs CSI
 * against [applicationCursorKeys] like any cursor key.
 */
@Composable
private fun ArrowPad(
    applicationCursorKeys: Boolean,
    onKey: (String) -> Unit,
    onPaste: () -> Unit,
    onAttachFromGallery: () -> Unit,
    onAttachFromClipboard: (Uri) -> Unit,
) {
    Row(
        modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 2.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
            ArrowButton(ArrowLeft, applicationCursorKeys, onKey)
            ArrowButton(ArrowUp, applicationCursorKeys, onKey)
            ArrowButton(ArrowDown, applicationCursorKeys, onKey)
            ArrowButton(ArrowRight, applicationCursorKeys, onKey)
        }
        Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
            AttachButton(onFromGallery = onAttachFromGallery, onFromClipboard = onAttachFromClipboard)
            OutlinedButton(onClick = onPaste, contentPadding = KEY_BAR_BUTTON_PADDING) {
                Text(stringResource(R.string.terminal_paste), maxLines = 1)
            }
        }
    }
}

/** Narrower than Material's default so the D-pad, attach and Paste share one row at phone width. */
internal val KEY_BAR_BUTTON_PADDING = PaddingValues(horizontal = 18.dp, vertical = 6.dp)

@Composable
private fun ArrowButton(key: CursorKey, applicationCursorKeys: Boolean, onKey: (String) -> Unit) {
    val description = stringResource(key.contentDescriptionRes)
    OutlinedButton(
        onClick = { onKey(key.sequence(applicationCursorKeys)) },
        contentPadding = KEY_BAR_BUTTON_PADDING,
        modifier = Modifier.semantics { contentDescription = description },
    ) { Text(key.label) }
}

/**
 * The horizontally-scrolling key bar, with the latching Ctrl chip pinned
 * outside the scroll (like the D-pad, it must never require scrolling to
 * find). [ctrlArmed] is owned by the caller so the same arm/disarm state
 * also gates typed-letter input outside this composable.
 */
/**
 * Makes a vertical drag across these buttons cancel the tap instead of firing
 * the key on lift-off.
 *
 * Compose ends a tap only when something CONSUMES the movement -- distance
 * alone never cancels one. A horizontal drag here is consumed by the bar's own
 * horizontalScroll, so it already behaves; a vertical drag is consumed by
 * nothing, so a scroll swipe that starts on a key button still sends that key.
 * On `Esc` that is a bare ESC, which interrupts a running agent and opens the
 * rewind overlay on the second one (cmux-app-qts).
 *
 * Claims on the Initial pass, so the buttons see the event already consumed
 * rather than one event later, and only once the drag is past touch slop and
 * vertical-dominant -- below that it is still a tap, and horizontal movement
 * still belongs to the scroll.
 */
private fun Modifier.cancelTapOnVerticalDrag(): Modifier = pointerInput(Unit) {
    val slop = viewConfiguration.touchSlop
    awaitEachGesture {
        val down = awaitFirstDown(requireUnconsumed = false, pass = PointerEventPass.Initial)
        var travel = Offset.Zero
        while (true) {
            val event = awaitPointerEvent(PointerEventPass.Initial)
            val change = event.changes.firstOrNull { it.id == down.id && it.pressed } ?: break
            travel += change.positionChangeIgnoreConsumed()
            if (abs(travel.y) > slop && abs(travel.y) > abs(travel.x)) change.consume()
        }
    }
}

@Composable
private fun KeyBar(
    applicationCursorKeys: Boolean,
    ctrlArmed: Boolean,
    onToggleCtrl: () -> Unit,
    onKey: (String) -> Unit,
) {
    Row(
        modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp),
        horizontalArrangement = Arrangement.spacedBy(6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        CtrlChip(armed = ctrlArmed, onClick = onToggleCtrl)
        val keyScroll = rememberScrollState()
        Row(
            // At 360dp only a sliver of the next key peeked past the edge, which
            // reads as the bar simply ending -- PgUp/PgDn/^D/^Z and the F-keys
            // went undiscovered. The fade says the row continues, and it appears
            // only on a side that actually has more.
            modifier = Modifier
                .cancelTapOnVerticalDrag()
                .scrollEdgeFade(keyScroll)
                .horizontalScroll(keyScroll),
            horizontalArrangement = Arrangement.spacedBy(6.dp),
        ) {
            TerminalKeys.forEach { key ->
                val description = stringResource(key.contentDescriptionRes)
                OutlinedButton(
                    onClick = { onKey(key.sequence(applicationCursorKeys)) },
                    modifier = Modifier.semantics { contentDescription = description },
                ) {
                    Text(key.label)
                }
            }
        }
    }
}

/** Width of the fade at each end of a scrollable row. */
private val ScrollEdgeFadeWidth = 24.dp

/**
 * Fades whichever end of a horizontally scrollable row still has content past
 * it. Must sit *before* [horizontalScroll] in the chain so it wraps the clipped
 * viewport rather than the scrolling content, and needs an offscreen
 * compositing layer because [BlendMode.DstIn] has to see the row's own pixels
 * to erase them.
 */
private fun Modifier.scrollEdgeFade(scroll: ScrollState): Modifier = this
    .graphicsLayer { compositingStrategy = CompositingStrategy.Offscreen }
    .drawWithContent {
        drawContent()
        val fade = ScrollEdgeFadeWidth.toPx()
        if (scroll.value > 0) {
            drawRect(
                brush = Brush.horizontalGradient(
                    listOf(Color.Transparent, Color.Black),
                    startX = 0f,
                    endX = fade,
                ),
                blendMode = BlendMode.DstIn,
            )
        }
        if (scroll.value < scroll.maxValue) {
            drawRect(
                brush = Brush.horizontalGradient(
                    listOf(Color.Black, Color.Transparent),
                    startX = size.width - fade,
                    endX = size.width,
                ),
                blendMode = BlendMode.DstIn,
            )
        }
    }

/**
 * The general Ctrl modifier: tapping it arms sending the next key as its
 * Ctrl combination (see [applyCtrlArm]) instead of its literal form, then
 * disarms itself. Filled with the primary color while armed -- distinct
 * enough at a glance that a modal toggle doesn't get left on unnoticed. The
 * scrollable bar's own ^C/^D/^Z stay as one-tap shortcuts for the combos
 * used often enough to be worth a dedicated button; this chip covers
 * everything else (Ctrl+L, Ctrl+A/E, Ctrl+R, ...) without hardcoding a
 * button per combo.
 */
@Composable
private fun CtrlChip(armed: Boolean, onClick: () -> Unit) {
    val colors = if (armed) {
        ButtonDefaults.outlinedButtonColors(
            containerColor = MaterialTheme.colorScheme.primary,
            contentColor = MaterialTheme.colorScheme.onPrimary,
        )
    } else {
        ButtonDefaults.outlinedButtonColors()
    }
    val ctrlContentDescription = stringResource(R.string.terminal_ctrl_content_description)
    val armedStateDescription = stringResource(
        if (armed) R.string.terminal_ctrl_state_armed else R.string.terminal_ctrl_state_not_armed,
    )
    OutlinedButton(
        onClick = onClick,
        colors = colors,
        // The armed/unarmed distinction is otherwise color-only (filled vs
        // outlined) -- stateDescription carries it to TalkBack without
        // changing the button's role away from the plain "double tap to
        // activate" hint that matches its actual one-shot-per-tap behavior.
        modifier = Modifier.semantics {
            contentDescription = ctrlContentDescription
            stateDescription = armedStateDescription
        },
    ) { Text(stringResource(R.string.terminal_ctrl_chip)) }
}
