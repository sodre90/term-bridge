package com.sodre90.cmuxremote.ui.layout

import com.sodre90.cmuxremote.model.LayoutPane
import com.sodre90.cmuxremote.model.PanePlacement
import com.sodre90.cmuxremote.model.WorkspaceLayout

/** A rectangle in the unit square the workspace's panes are drawn into. */
data class MiniRect(val x: Float, val y: Float, val w: Float, val h: Float) {
    val right: Float get() = x + w
    val bottom: Float get() = y + h

    fun contains(px: Float, py: Float): Boolean = px >= x && px < right && py >= y && py < bottom
}

/** One pane of the miniature, its rectangle already shrunk to make room
 *  for the ghost when it is the target. */
data class MiniaturePane(
    val id: String,
    val rect: MiniRect,
    val surfaceIds: List<String>,
    val selectedSurfaceId: String,
    val isTarget: Boolean,
)

/**
 * The picture the placement sheet draws: every pane of the workspace at its
 * fraction, the [ghost] the new pane will occupy for a split, or the
 * [tabStrip] the target pane grows for a tab (its last entry is the new tab).
 * [targetSurfaceId] is the surface the split or tab is asked of.
 */
data class Miniature(
    val panes: List<MiniaturePane>,
    val targetSurfaceId: String?,
    val ghost: MiniRect?,
    val tabStrip: List<MiniRect>,
    val estimated: Boolean,
)

private const val HALF = 0.5f
private const val TAB_STRIP_HEIGHT = 0.08f
private const val TAB_MAX_WIDTH = 0.18f
private const val TAB_MAX_HEIGHT_SHARE = 0.3f

/**
 * The pane a split or tab will act on: the one holding [surfaceId] when the
 * layout still has it, else the pane cmux has focused, else the first --
 * the fallbacks cover a workspace opened from the list, and a surface
 * closed on the Mac since the list was fetched.
 */
fun targetPane(layout: WorkspaceLayout, surfaceId: String?): LayoutPane? =
    layout.panes.firstOrNull { surfaceId != null && surfaceId in it.surfaceIds }
        ?: layout.panes.firstOrNull { it.focused }
        ?: layout.panes.firstOrNull()

/**
 * Lays the workspace out with the new pane in place, the way cmux splits:
 * the target pane keeps one half along the split axis and the ghost takes
 * the half on the [placement]'s side. A tab changes no rectangle.
 */
fun miniature(layout: WorkspaceLayout, surfaceId: String?, placement: String?): Miniature {
    val target = targetPane(layout, surfaceId)
    val targetRect = target?.let(::rectOf)
    val direction = placement?.takeIf { it in SPLIT_PLACEMENTS }
    val split = if (targetRect != null && direction != null) halves(targetRect, direction) else null
    val tabStrip = if (placement == PanePlacement.TAB && target != null && targetRect != null) {
        tabStrip(targetRect, target.surfaceIds.size + 1)
    } else {
        emptyList()
    }
    return Miniature(
        panes = layout.panes.map { pane ->
            MiniaturePane(
                id = pane.id,
                rect = if (pane === target && split != null) split.kept else rectOf(pane),
                surfaceIds = pane.surfaceIds,
                selectedSurfaceId = pane.selectedSurfaceId,
                isTarget = pane === target,
            )
        },
        targetSurfaceId = target?.let { surfaceIn(it, surfaceId) },
        ghost = split?.freed,
        tabStrip = tabStrip,
        estimated = layout.estimated,
    )
}

/** The surface a request names within [pane]: the one asked for when the
 *  pane holds it, else the pane's selected tab. */
fun surfaceIn(pane: LayoutPane, surfaceId: String?): String? =
    surfaceId?.takeIf { it in pane.surfaceIds }
        ?: pane.selectedSurfaceId.ifBlank { pane.surfaceIds.firstOrNull() }

private val SPLIT_PLACEMENTS = setOf(PanePlacement.LEFT, PanePlacement.RIGHT, PanePlacement.UP, PanePlacement.DOWN)

private data class Split(val kept: MiniRect, val freed: MiniRect)

/** Where the ghost starts its slide: a zero-thickness slice at the edge
 *  the new pane enters from, so [ghost] is what it grows into. */
fun ghostEntryEdge(ghost: MiniRect, placement: String): MiniRect = when (placement) {
    PanePlacement.LEFT -> MiniRect(ghost.x, ghost.y, 0f, ghost.h)
    PanePlacement.RIGHT -> MiniRect(ghost.right, ghost.y, 0f, ghost.h)
    PanePlacement.UP -> MiniRect(ghost.x, ghost.y, ghost.w, 0f)
    else -> MiniRect(ghost.x, ghost.bottom, ghost.w, 0f)
}

private fun rectOf(pane: LayoutPane) =
    MiniRect(pane.x.toFloat(), pane.y.toFloat(), pane.w.toFloat(), pane.h.toFloat())

private fun halves(rect: MiniRect, placement: String): Split {
    val halfW = rect.w * HALF
    val halfH = rect.h * HALF
    val left = MiniRect(rect.x, rect.y, halfW, rect.h)
    val right = MiniRect(rect.x + halfW, rect.y, halfW, rect.h)
    val top = MiniRect(rect.x, rect.y, rect.w, halfH)
    val bottom = MiniRect(rect.x, rect.y + halfH, rect.w, halfH)
    return when (placement) {
        PanePlacement.LEFT -> Split(kept = right, freed = left)
        PanePlacement.RIGHT -> Split(kept = left, freed = right)
        PanePlacement.UP -> Split(kept = bottom, freed = top)
        else -> Split(kept = top, freed = bottom)
    }
}

private fun tabStrip(pane: MiniRect, count: Int): List<MiniRect> {
    val height = minOf(TAB_STRIP_HEIGHT, pane.h * TAB_MAX_HEIGHT_SHARE)
    val width = minOf(TAB_MAX_WIDTH, pane.w / count)
    return List(count) { i -> MiniRect(pane.x + i * width, pane.y, width, height) }
}
