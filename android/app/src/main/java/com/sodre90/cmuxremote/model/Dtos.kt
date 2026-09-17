package com.sodre90.cmuxremote.model

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject

/**
 * Shared JSON codec for every value exchanged with the cmux bridge.
 *
 * - [ignoreUnknownKeys]: the bridge (and cmux behind it) may grow fields; the
 *   client must keep parsing older shapes.
 * - [explicitNulls] = false: omit null fields when we encode upstream messages.
 */
val BridgeJson: Json = Json {
    ignoreUnknownKeys = true
    encodeDefaults = false
    explicitNulls = false
}

/** A cmux workspace as surfaced by the bridge `GET /sessions` endpoint. */
@Serializable
data class Workspace(
    val id: String = "",
    val cwd: String = "",
    val title: String = "",
    val preview: String = "",
    @SerialName("has_unread") val hasUnread: Boolean = false,
    /** Agent attention state derived by the bridge (see [Attention]); "" means none. */
    val attention: String = "",
    /** This workspace's YOLO auto-reply mode (see [YoloMode]); "" means off. */
    @SerialName("yolo_mode") val yoloMode: String = "",
    /** The workspace's user-picked color in cmux (`#rrggbb`); "" when unset.
     *  Rendered as an identifying dot on the sessions card. */
    @SerialName("custom_color") val customColor: String = "",
    val terminals: List<TerminalPane> = emptyList(),
)

/**
 * [Workspace.attention]'s possible values -- the agent-attention state the
 * bridge derives per workspace: blocked on a permission prompt, idle waiting
 * for input, or [NONE].
 */
object Attention {
    const val NONE = ""
    const val PERMISSION = "permission"
    const val INPUT = "input"
}

/**
 * YOLO mode's auto-reply levels for permission prompts. The bridge persists
 * these per workspace and replies to pending `permission`-kind feed items
 * with them automatically (see bridge's internal/yolo package); [BYPASS]
 * mirrors Claude Code's own `--dangerously-skip-permissions`.
 */
object YoloMode {
    const val OFF = ""
    const val ALWAYS = "always"
    const val ALL_TOOLS = "all"
    const val BYPASS = "bypass"
}

/** One terminal surface (pane) within a [Workspace]; [id] opens via /terminal/{id}. */
@Serializable
data class TerminalPane(
    val id: String = "",
    val cwd: String = "",
    val title: String = "",
    val focused: Boolean = false,
    val ready: Boolean = false,
    val kind: String = "",
)

/** Envelope returned by `GET /sessions`; mirrors bridge/internal/wire/sessions.go's
 *  SessionsResponse. [host] defaults to a cmux host so a bridge too old to
 *  send it behaves exactly as before. [pendingCount] is how many prompts the
 *  Inbox would list; null when the bridge could not say (no feed, feed
 *  unreadable, or too old), in which case the badge is fetched the old way. */
@Serializable
data class WorkspacesResponse(
    val workspaces: List<Workspace> = emptyList(),
    val host: HostInfo = HostInfo(),
    @SerialName("pending_count") val pendingCount: Int? = null,
)

/** Which machine and backend an agent fronts (wire HostInfo). [kind] is one of
 *  [HostKind]; [name] the host's short hostname. */
@Serializable
data class HostInfo(
    val name: String = "",
    val kind: String = HostKind.CMUX,
    val capabilities: HostCapabilities = HostCapabilities(),
)

object HostKind {
    const val CMUX = "cmux"
    const val TMUX = "tmux"
}

/** What the host can do, so the app offers only that (wire HostCapabilities).
 *  Defaults describe cmux: tabs exist and the Inbox has structured prompts. */
@Serializable
data class HostCapabilities(
    /** A pane holds several surfaces; "add as tab" placement is offered. */
    val tabs: Boolean = true,
    /** The Inbox carries structured prompts with replies for this host. */
    val feed: Boolean = true,
)

/** Body of `GET /version` -- mirrors bridge/internal/wire/version.go's
 *  VersionResponse. Defaulted so an agent too old to serve the route (or one
 *  answering without the field) decodes to an empty string rather than
 *  throwing, which the Connections screen renders as "unknown". */
@Serializable
data class VersionResponse(val bridge: String = "")

/** Body of `POST /devices/register`. */
@Serializable
data class RegisterDeviceRequest(@SerialName("fcm_token") val fcmToken: String)

/** Body of `POST /sessions/{id}/rename`. */
@Serializable
data class RenameWorkspaceRequest(val title: String)

/** Body of `POST /sessions/{id}/yolo-mode`; [mode] is one of [YoloMode]'s values. */
@Serializable
data class SetYoloModeRequest(val mode: String)

/** Body of `POST /sessions`: a new workspace in [cwd] on the Mac (absolute,
 *  under the home directory -- the bridge checks). Empty [title] lets cmux
 *  pick one. Mirrors `CreateWorkspaceRequest` in bridge/internal/wire/layout.go. */
@Serializable
data class CreateWorkspaceRequest(val cwd: String, val title: String? = null)

/** Reply to `POST /sessions`: the new workspace and its first terminal. */
@Serializable
data class CreateWorkspaceResponse(
    @SerialName("workspace_id") val workspaceId: String = "",
    @SerialName("surface_id") val surfaceId: String = "",
)

/** Body of `POST /sessions/{id}/panes`: a new terminal placed relative to
 *  [surfaceId], the surface being viewed; [placement] is one of
 *  [PanePlacement]'s values. */
@Serializable
data class CreatePaneRequest(@SerialName("surface_id") val surfaceId: String, val placement: String)

/** Reply to `POST /sessions/{id}/panes`. */
@Serializable
data class CreatePaneResponse(
    @SerialName("surface_id") val surfaceId: String = "",
    @SerialName("pane_id") val paneId: String = "",
)

/** [CreatePaneRequest.placement]'s values -- mirrors the `Placement*`
 *  constants in bridge/internal/wire/layout.go. The four directions split
 *  the viewed surface's pane; [TAB] adds a tab to it. */
object PanePlacement {
    const val LEFT = "left"
    const val RIGHT = "right"
    const val UP = "up"
    const val DOWN = "down"
    const val TAB = "tab"
}

/** Body of `POST /sessions/{id}/select`: show the workspace on the Mac and,
 *  when set, focus [surfaceId] in it. */
@Serializable
data class SelectWorkspaceRequest(@SerialName("surface_id") val surfaceId: String? = null)

/** Reply to `GET /sessions/{id}/layout`: where each pane sits, as fractions
 *  of the panes' bounding box. [estimated] means cmux had no geometry for
 *  the workspace yet and the panes are equal columns in index order. Mirrors
 *  `Layout` in bridge/internal/wire/layout.go. */
@Serializable
data class WorkspaceLayout(val estimated: Boolean = false, val panes: List<LayoutPane> = emptyList())

/** One pane in a [WorkspaceLayout]; [x], [y], [w], [h] are in 0..1. */
@Serializable
data class LayoutPane(
    val id: String = "",
    val x: Double = 0.0,
    val y: Double = 0.0,
    val w: Double = 1.0,
    val h: Double = 1.0,
    val focused: Boolean = false,
    @SerialName("surface_ids") val surfaceIds: List<String> = emptyList(),
    @SerialName("selected_surface_id") val selectedSurfaceId: String = "",
)

/** A simplified event the bridge fans out over the events WebSocket. */
@Serializable
data class EventFrame(
    val type: String = "",
    val name: String = "",
    @SerialName("needs_attention") val needsAttention: Boolean = false,
    @SerialName("feed_id") val feedId: String? = null,
    @SerialName("workspace_id") val workspaceId: String? = null,
    @SerialName("surface_id") val surfaceId: String? = null,
    val title: String? = null,
    val kind: String? = null,
)

/**
 * A terminal frame pushed down to the client: a [TerminalDownType.REPLAY]/
 * [TerminalDownType.OUTPUT] render-grid snapshot, or an [TerminalDownType.ACK]
 * echoing a [TerminalUp.seq] back with [ok] reflecting whether the bridge's
 * cmux RPC for that message actually succeeded.
 */
@Serializable
data class TerminalDown(
    val type: String = "",
    val grid: RenderGrid? = null,
    val columns: Int = 0,
    val rows: Int = 0,
    val seq: Long = 0L,
    val ok: Boolean = false,
    /** Why an ack is not [ok] when the bridge itself refused the message
     *  rather than cmux failing it; one of [AttachRefusal]'s values for an
     *  attach, absent otherwise. Mirrors `Reason` in
     *  bridge/internal/wire/terminal.go. */
    val reason: String? = null,
    /** Render-grid blocks the bridge left out of [grid] because they are
     *  identical to the ones it already sent; this frame's grid is completed
     *  from the previous one (see [mergedOnto]). Only ever set on output frames,
     *  and only once the delta handshake succeeded. Mirrors `Unchanged` in
     *  bridge/internal/wire/terminal.go. */
    val unchanged: List<String> = emptyList(),
    /** The visible rows whose spans [grid]'s `row_spans` carries; every other
     *  row is kept from the previous frame and a listed row with no spans has
     *  emptied (see [mergedOnto]). Empty means `row_spans` is whole. Only ever
     *  set on output frames of a socket opened with `rows=1`. Mirrors
     *  `RowsChanged` in bridge/internal/wire/terminal.go. */
    @SerialName("rows_changed")
    val rowsChanged: List<Int> = emptyList(),
)

/** Block names that can appear in [TerminalDown.unchanged] -- mirrors
 *  `stickyGridFields` (and `rowSpansBlock`, named only when no visible row
 *  changed on a `rows=1` socket) in bridge/internal/server/terminal.go. The
 *  two theme blocks are also omitted there, but this model never parsed them,
 *  so only the scrollback needs carrying forward here. */
object UnchangedBlock {
    const val SCROLLBACK_SPANS = "scrollback_spans"
    const val STYLES = "styles"
    const val MODES = "modes"
    const val ROW_SPANS = "row_spans"
}

/**
 * [TerminalDown.type]'s possible values. Plain `String` (not an enum) on the
 * DTO: an unrecognized future frame type must still decode instead of
 * failing the whole socket, matching TerminalViewModel's existing
 * fallthrough (anything that isn't [ACK] is treated as a render-grid frame
 * if it carries one, ignored otherwise).
 */
object TerminalDownType {
    const val ACK = "ack"
    const val REPLAY = "replay"
    const val OUTPUT = "output"
}

/**
 * A terminal message sent up from the client: [TerminalUpType.INPUT],
 * [TerminalUpType.PASTE], [TerminalUpType.RESIZE] or [TerminalUpType.ATTACH].
 * [seq] is a client-assigned monotonic id (starting at 1; 0 means unset)
 * echoed back in the matching [TerminalDownType.ACK] [TerminalDown].
 *
 * A paste carries [text] the host delivers as one paste rather than as
 * typing -- bracketed (ESC[200~ ... ESC[201~) exactly when the pane has asked
 * for that, which only the host owning the PTY can know reliably.
 *
 * An attach carries [image], the file's bytes in base64, which the bridge
 * writes to disk and pastes the path of into the pane. [name] is only a hint
 * for the bridge's log; it never becomes part of the path.
 */
@Serializable
data class TerminalUp(
    val type: String,
    val text: String? = null,
    val columns: Int? = null,
    val rows: Int? = null,
    val seq: Long = 0L,
    val image: String? = null,
    val name: String? = null,
)

/** [TerminalUp.type]'s possible values -- what this client ever sends. */
object TerminalUpType {
    const val INPUT = "input"
    const val PASTE = "paste"
    const val RESIZE = "resize"
    const val ATTACH = "attach"
}

/** [TerminalDown.reason]'s values on a refused attach -- mirrors
 *  `attachRefusalReason` in bridge/internal/server/attachments.go. */
object AttachRefusal {
    const val TOO_LARGE = "too_large"
    const val NOT_IMAGE = "not_image"
    const val ATTACHMENTS_OFF = "attachments_off"
    const val BAD_ENCODING = "bad_encoding"
}

/** A reply to a pending feed item (permission / question / exit-plan). */
@Serializable
data class FeedReply(
    val kind: String,
    @SerialName("request_id") val requestId: String,
    val params: JsonObject,
)

/** One selectable choice within a [FeedQuestion]. cmux replies with the [label]. */
@Serializable
data class FeedOption(
    val id: String = "",
    val label: String = "",
    val description: String = "",
)

/** A single question inside an AskUserQuestion prompt; an item may carry several. */
@Serializable
data class FeedQuestion(
    val id: String = "",
    val header: String = "",
    val prompt: String = "",
    @SerialName("multi_select") val multiSelect: Boolean = false,
    val options: List<FeedOption> = emptyList(),
)

/**
 * A pending blocking prompt from `GET /feed/pending` (cmux `feed.list pending_only`).
 * [requestId] is what [FeedReply] must echo back — not the event feed id. For
 * `kind == "question"` the reply is `selections`: the chosen options' labels.
 * For `kind == "permissionRequest"` there is no questions[]/options[] — instead
 * [toolName]/[toolInput] describe the gated tool call, and the reply is a
 * `mode` of `once` or `deny` (confirmed live against `feed.permission.reply`;
 * `always`/`all`/`bypass` are the same enum's YOLO auto-modes, see [YoloMode]).
 * [toolInput] is cmux's own JSON-encoded string of the tool's raw args (shape
 * varies per tool), not a typed object.
 */
@Serializable
data class PendingFeedItem(
    val id: String = "",
    @SerialName("request_id") val requestId: String = "",
    val kind: String = "",
    val status: String = "",
    val title: String = "",
    val cwd: String = "",
    @SerialName("workstream_id") val workstreamId: String = "",
    @SerialName("question_multi_select") val questionMultiSelect: Boolean = false,
    @SerialName("question_options") val questionOptions: List<FeedOption> = emptyList(),
    val questions: List<FeedQuestion> = emptyList(),
    @SerialName("tool_name") val toolName: String = "",
    @SerialName("tool_input") val toolInput: String = "",
)

/** Envelope returned by `GET /feed/pending`. */
@Serializable
data class PendingFeedResponse(val items: List<PendingFeedItem> = emptyList())
