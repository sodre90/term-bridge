package com.sodre90.cmuxremote.ui.layout

import androidx.activity.compose.BackHandler
import androidx.compose.animation.core.Animatable
import androidx.compose.animation.core.AnimationVector4D
import androidx.compose.animation.core.TwoWayConverter
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.ModalBottomSheetProperties
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import com.sodre90.cmuxremote.R
import com.sodre90.cmuxremote.model.PanePlacement
import com.sodre90.cmuxremote.ui.sessions.actionOutcomeText

/**
 * Where a new pane goes, shown before it exists: the workspace's panes in
 * miniature, a ghost pane sliding into the half a split would free, or an
 * extra tab on the pane a tab would join. The button says what will happen.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun PlacementSheet(
    state: PlacementState,
    initialSurfaceId: String?,
    onCreate: (surfaceId: String, placement: String) -> Unit,
    onDismiss: () -> Unit,
) {
    val navigationBar = rootNavigationBarBottom()
    ModalBottomSheet(
        onDismissRequest = onDismiss,
        properties = ModalBottomSheetProperties(shouldDismissOnBackPress = false),
        contentWindowInsets = { WindowInsets(0) },
    ) {
        // Back is answered here, not by the sheet: on API 35 the sheet's own
        // handler ran its hide animation partway and stopped, leaving the sheet
        // on screen shifted down, with a second press needed to close it.
        BackHandler(onBack = onDismiss)
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 24.dp)
                .padding(bottom = SHEET_BOTTOM_PADDING + navigationBar),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            Text(stringResource(R.string.placement_title), style = MaterialTheme.typography.titleLarge)
            when (state) {
                PlacementState.Loading -> Box(
                    modifier = Modifier.fillMaxWidth().height(MINIATURE_MIN_HEIGHT),
                    contentAlignment = Alignment.Center,
                ) { CircularProgressIndicator() }
                is PlacementState.Failed -> {
                    Text(actionOutcomeText(state.outcome), color = MaterialTheme.colorScheme.error)
                    TextButton(onClick = onDismiss, modifier = Modifier.align(Alignment.End)) {
                        Text(stringResource(R.string.action_close))
                    }
                }
                is PlacementState.Ready -> PlacementPicker(state, initialSurfaceId, onCreate)
            }
        }
    }
}

@Composable
private fun PlacementPicker(
    state: PlacementState.Ready,
    initialSurfaceId: String?,
    onCreate: (surfaceId: String, placement: String) -> Unit,
) {
    var surfaceId by rememberSaveable { mutableStateOf(initialSurfaceId) }
    var placement by rememberSaveable { mutableStateOf(PanePlacement.RIGHT) }
    val miniature = miniature(state.layout, surfaceId, placement)

    WorkspaceMiniature(miniature, placement, state.titles, onSelectPane = { surfaceId = it })
    if (miniature.estimated) {
        Text(
            stringResource(R.string.placement_estimated_note),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
    PlacementChips(placement, offered = offeredPlacements(state.tabs), onSelect = { placement = it })
    state.error?.let { Text(actionOutcomeText(it), color = MaterialTheme.colorScheme.error) }
    val target = miniature.targetSurfaceId
    if (target == null) {
        Text(stringResource(R.string.placement_no_panes), color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
    Button(
        onClick = { target?.let { onCreate(it, placement) } },
        enabled = target != null && !state.busy,
        modifier = Modifier.fillMaxWidth(),
    ) {
        Text(stringResource(placementLabel(placement)))
    }
}

/**
 * The navigation bar's height from the root window, which nothing consumes.
 * Observed with the sheet's default insets: bottom padding present on the
 * first open, gone once the sheet had been dismissed and reopened, sinking
 * the button under the bar. The composition-level inset is no use either,
 * as the screens' Scaffold has already consumed it.
 */
@Composable
private fun rootNavigationBarBottom(): Dp {
    val view = LocalView.current
    val insets = ViewCompat.getRootWindowInsets(view) ?: return 0.dp
    val bottom = insets.getInsets(WindowInsetsCompat.Type.navigationBars()).bottom
    return with(LocalDensity.current) { bottom.toDp() }
}

private val SHEET_BOTTOM_PADDING = 24.dp
private const val MINIATURE_ASPECT = 16f / 10f
private val MINIATURE_MIN_HEIGHT = 160.dp
private const val PLACEMENT_ANIM_MS = 250
private val PANE_GAP = 3.dp
private val PANE_CORNER = 4.dp
private const val GHOST_FILL_ALPHA = 0.25f

/**
 * The panes as offset boxes over a fixed-aspect canvas; each edge animates
 * to its new fraction, so a split visibly makes room. Compose scales these
 * by the system animator setting, so a user who turned animations off gets
 * the end state at once.
 */
@Composable
private fun WorkspaceMiniature(
    miniature: Miniature,
    placement: String,
    titles: Map<String, String>,
    onSelectPane: (surfaceId: String) -> Unit,
) {
    BoxWithConstraints(
        modifier = Modifier
            .fillMaxWidth()
            .aspectRatio(MINIATURE_ASPECT)
            .clip(RoundedCornerShape(8.dp))
            .background(MaterialTheme.colorScheme.surfaceContainerHighest),
    ) {
        val width = maxWidth
        val height = maxHeight
        miniature.panes.forEachIndexed { index, pane ->
            key(pane.id) {
                PaneBox(
                    rect = animatedRect(pane.rect),
                    width = width,
                    height = height,
                    pane = pane,
                    title = titles[pane.selectedSurfaceId].orEmpty(),
                    ordinal = index + 1,
                    estimated = miniature.estimated,
                    onSelect = { selectedSurfaceOf(pane)?.let(onSelectPane) },
                )
            }
        }
        GhostBox(miniature.ghost, placement, width, height)
        miniature.tabStrip.forEachIndexed { i, tab ->
            val isNew = i == miniature.tabStrip.lastIndex
            Box(
                modifier = Modifier
                    .offset(x = width * tab.x, y = height * tab.y)
                    .size(width * tab.w, height * tab.h)
                    .padding(PANE_GAP)
                    .clip(RoundedCornerShape(topStart = PANE_CORNER, topEnd = PANE_CORNER))
                    .background(
                        if (isNew) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant,
                    ),
            )
        }
    }
}

private fun selectedSurfaceOf(pane: MiniaturePane): String? =
    pane.selectedSurfaceId.ifBlank { pane.surfaceIds.firstOrNull() }

@Composable
private fun animatedRect(target: MiniRect): MiniRect {
    val x by animateFloatAsState(target.x, tween(PLACEMENT_ANIM_MS), label = "paneX")
    val y by animateFloatAsState(target.y, tween(PLACEMENT_ANIM_MS), label = "paneY")
    val w by animateFloatAsState(target.w, tween(PLACEMENT_ANIM_MS), label = "paneW")
    val h by animateFloatAsState(target.h, tween(PLACEMENT_ANIM_MS), label = "paneH")
    return MiniRect(x, y, w, h)
}

@Composable
private fun PaneBox(
    rect: MiniRect,
    width: Dp,
    height: Dp,
    pane: MiniaturePane,
    title: String,
    ordinal: Int,
    estimated: Boolean,
    onSelect: () -> Unit,
) {
    val fill = if (pane.isTarget) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surface
    val ink = if (pane.isTarget) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurface
    val outline = MaterialTheme.colorScheme.outline
    val description = stringResource(R.string.placement_pane_description, title.ifBlank { ordinal.toString() })
    Box(
        modifier = Modifier
            .offset(x = width * rect.x, y = height * rect.y)
            .size(width * rect.w, height * rect.h)
            .padding(PANE_GAP)
            .clip(RoundedCornerShape(PANE_CORNER))
            .background(fill)
            .then(
                if (estimated) {
                    Modifier.dashedBorder(outline)
                } else {
                    Modifier.border(1.dp, outline, RoundedCornerShape(PANE_CORNER))
                },
            )
            .clickable(onClick = onSelect)
            .semantics { contentDescription = description }
            .padding(4.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            title,
            style = MaterialTheme.typography.labelSmall,
            color = ink,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
        )
    }
}

private val MiniRectConverter = TwoWayConverter<MiniRect, AnimationVector4D>(
    convertToVector = { AnimationVector4D(it.x, it.y, it.w, it.h) },
    convertFromVector = { MiniRect(it.v1, it.v2, it.v3, it.v4) },
)

/**
 * The pane-to-be. First appearance slides in from the edge cmux would open
 * it at; a change of direction or target glides from wherever the ghost
 * already is, so the picture never restarts.
 */
@Composable
private fun GhostBox(ghost: MiniRect?, placement: String, width: Dp, height: Dp) {
    val rect = remember { Animatable(MiniRect(0f, 0f, 0f, 0f), MiniRectConverter) }
    var shown by remember { mutableStateOf(false) }
    LaunchedEffect(ghost, placement) {
        if (ghost == null) {
            shown = false
            return@LaunchedEffect
        }
        if (!shown) rect.snapTo(ghostEntryEdge(ghost, placement))
        shown = true
        rect.animateTo(ghost, tween(PLACEMENT_ANIM_MS))
    }
    if (!shown) return
    val accent = MaterialTheme.colorScheme.primary
    val description = stringResource(R.string.placement_new_pane_description)
    val current = rect.value
    Box(
        modifier = Modifier
            .offset(x = width * current.x, y = height * current.y)
            .size(width * current.w, height * current.h)
            .padding(PANE_GAP)
            .clip(RoundedCornerShape(PANE_CORNER))
            .background(accent.copy(alpha = GHOST_FILL_ALPHA))
            .dashedBorder(accent)
            .semantics { contentDescription = description },
        contentAlignment = Alignment.Center,
    ) {
        Icon(Icons.Default.Add, contentDescription = null, tint = accent)
    }
}

private fun Modifier.dashedBorder(color: Color): Modifier = drawBehind {
    val stroke = Stroke(
        width = 1.dp.toPx(),
        pathEffect = PathEffect.dashPathEffect(floatArrayOf(4.dp.toPx(), 3.dp.toPx())),
    )
    drawRoundRect(color = color, style = stroke, cornerRadius = CornerRadius(PANE_CORNER.toPx()))
}

private val CHIP_ORDER = listOf(
    PanePlacement.LEFT,
    PanePlacement.UP,
    PanePlacement.DOWN,
    PanePlacement.RIGHT,
    PanePlacement.TAB,
)

/** The placements a host can honour: every split, plus a tab only where the
 *  host has tabs (see HostCapabilities.tabs). */
internal fun offeredPlacements(tabs: Boolean): List<String> =
    if (tabs) CHIP_ORDER else CHIP_ORDER.filterNot { it == PanePlacement.TAB }

@Composable
private fun PlacementChips(selected: String, offered: List<String>, onSelect: (String) -> Unit) {
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.spacedBy(8.dp, Alignment.CenterHorizontally),
    ) {
        offered.forEach { placement ->
            val description = stringResource(placementLabel(placement))
            FilterChip(
                selected = placement == selected,
                onClick = { onSelect(placement) },
                label = { Text(chipGlyph(placement)) },
                modifier = Modifier.semantics { contentDescription = description },
            )
        }
    }
}

@Composable
private fun chipGlyph(placement: String): String = when (placement) {
    PanePlacement.LEFT -> "←"
    PanePlacement.UP -> "↑"
    PanePlacement.DOWN -> "↓"
    PanePlacement.RIGHT -> "→"
    else -> stringResource(R.string.placement_tab_chip)
}

private fun placementLabel(placement: String): Int = when (placement) {
    PanePlacement.LEFT -> R.string.placement_split_left
    PanePlacement.UP -> R.string.placement_split_up
    PanePlacement.DOWN -> R.string.placement_split_down
    PanePlacement.RIGHT -> R.string.placement_split_right
    else -> R.string.placement_add_tab
}
