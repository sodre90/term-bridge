package com.sodre90.cmuxremote.data

import com.sodre90.cmuxremote.model.BridgeJson
import com.sodre90.cmuxremote.model.CreatePaneRequest
import com.sodre90.cmuxremote.model.CreatePaneResponse
import com.sodre90.cmuxremote.model.CreateWorkspaceRequest
import com.sodre90.cmuxremote.model.CreateWorkspaceResponse
import com.sodre90.cmuxremote.model.FeedReply
import com.sodre90.cmuxremote.model.PendingFeedItem
import com.sodre90.cmuxremote.model.PendingFeedResponse
import com.sodre90.cmuxremote.model.RegisterDeviceRequest
import com.sodre90.cmuxremote.model.RenameWorkspaceRequest
import com.sodre90.cmuxremote.model.SelectWorkspaceRequest
import com.sodre90.cmuxremote.model.SetYoloModeRequest
import com.sodre90.cmuxremote.model.VersionResponse
import com.sodre90.cmuxremote.model.WorkspaceLayout
import com.sodre90.cmuxremote.model.WorkspacesResponse
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.IOException

/** A non-2xx response from the bridge (e.g. Mac/cmux unavailable). */
class BridgeException(val code: Int, val bodyText: String) :
    IOException("bridge HTTP $code: $bodyText")

/**
 * REST calls against the bridge. The supplied [http] is expected to already
 * carry mTLS + the bearer token (see [Mtls.client]); this class only knows the
 * endpoint shapes. All calls run on [Dispatchers.IO].
 */
class BridgeClient(
    private val http: OkHttpClient,
    baseUrl: String,
) {
    private val root = baseUrl.trimEnd('/')

    suspend fun sessions(): WorkspacesResponse = withContext(Dispatchers.IO) {
        val request = Request.Builder().url("$root/sessions").get().build()
        http.newCall(request).execute().use { resp ->
            val body = resp.body?.string().orEmpty()
            if (!resp.isSuccessful) throw BridgeException(resp.code, body)
            BridgeJson.decodeFromString(WorkspacesResponse.serializer(), body)
        }
    }

    /** The agent's own version, for the Connections screen -- the app knows its
     *  own build from BuildConfig, but the two halves ship separately. */
    suspend fun version(): String = withContext(Dispatchers.IO) {
        val request = Request.Builder().url("$root/version").get().build()
        http.newCall(request).execute().use { resp ->
            val body = resp.body?.string().orEmpty()
            if (!resp.isSuccessful) throw BridgeException(resp.code, body)
            BridgeJson.decodeFromString(VersionResponse.serializer(), body).bridge
        }
    }

    suspend fun registerDevice(fcmToken: String) {
        val payload = BridgeJson.encodeToString(
            RegisterDeviceRequest.serializer(),
            RegisterDeviceRequest(fcmToken),
        )
        post("$root/devices/register", payload)
    }

    suspend fun pendingFeed(): List<PendingFeedItem> = withContext(Dispatchers.IO) {
        val request = Request.Builder().url("$root/feed/pending").get().build()
        http.newCall(request).execute().use { resp ->
            val body = resp.body?.string().orEmpty()
            if (!resp.isSuccessful) throw BridgeException(resp.code, body)
            BridgeJson.decodeFromString(PendingFeedResponse.serializer(), body).items
        }
    }

    suspend fun replyFeed(feedId: String, reply: FeedReply) {
        val payload = BridgeJson.encodeToString(FeedReply.serializer(), reply)
        post("$root/feed/$feedId/reply", payload)
    }

    /** Sets a workspace's persistent display title in cmux (see
     *  bridge/internal/server/rename.go's workspace.rename RPC call). */
    suspend fun renameWorkspace(id: String, title: String) {
        val payload = BridgeJson.encodeToString(RenameWorkspaceRequest.serializer(), RenameWorkspaceRequest(title))
        post("$root/sessions/$id/rename", payload)
    }

    /** Sets a workspace's YOLO auto-reply mode (see
     *  bridge/internal/server/yolo.go's handleSetYoloMode). */
    suspend fun setYoloMode(id: String, mode: String) {
        val payload = BridgeJson.encodeToString(SetYoloModeRequest.serializer(), SetYoloModeRequest(mode))
        post("$root/sessions/$id/yolo-mode", payload)
    }

    /** Creates a workspace in [cwd] on the Mac (see bridge/internal/server/
     *  workspaces.go's handleCreateWorkspace, which checks the directory);
     *  the reply names the workspace and its first terminal. */
    suspend fun createWorkspace(cwd: String, title: String?): CreateWorkspaceResponse {
        val payload = BridgeJson.encodeToString(
            CreateWorkspaceRequest.serializer(),
            CreateWorkspaceRequest(cwd = cwd, title = title?.takeIf { it.isNotBlank() }),
        )
        return BridgeJson.decodeFromString(CreateWorkspaceResponse.serializer(), postForBody("$root/sessions", payload))
    }

    /** A new terminal placed relative to [surfaceId] -- a split in one of the
     *  four directions or a tab in its pane (see bridge/internal/server/
     *  panes.go). [placement] is one of [com.sodre90.cmuxremote.model.PanePlacement]'s values. */
    suspend fun createPane(workspaceId: String, surfaceId: String, placement: String): CreatePaneResponse {
        val payload = BridgeJson.encodeToString(
            CreatePaneRequest.serializer(),
            CreatePaneRequest(surfaceId = surfaceId, placement = placement),
        )
        val body = postForBody("$root/sessions/$workspaceId/panes", payload)
        return BridgeJson.decodeFromString(CreatePaneResponse.serializer(), body)
    }

    /** Where a workspace's panes sit (see bridge/internal/server/layout.go). */
    suspend fun layout(workspaceId: String): WorkspaceLayout = withContext(Dispatchers.IO) {
        val request = Request.Builder().url("$root/sessions/$workspaceId/layout").get().build()
        http.newCall(request).execute().use { resp ->
            val body = resp.body?.string().orEmpty()
            if (!resp.isSuccessful) throw BridgeException(resp.code, body)
            BridgeJson.decodeFromString(WorkspaceLayout.serializer(), body)
        }
    }

    /** Makes the Mac show [workspaceId] and, when given, focus [surfaceId] in
     *  it (see bridge/internal/server/workspaces.go's handleSelectWorkspace). */
    suspend fun selectWorkspace(workspaceId: String, surfaceId: String? = null) {
        val payload = BridgeJson.encodeToString(SelectWorkspaceRequest.serializer(), SelectWorkspaceRequest(surfaceId))
        post("$root/sessions/$workspaceId/select", payload)
    }

    suspend fun closeWorkspace(workspaceId: String) = delete("$root/sessions/$workspaceId")

    suspend fun closeSurface(workspaceId: String, surfaceId: String) =
        delete("$root/sessions/$workspaceId/panes/$surfaceId")

    /** Triggers one real, end-to-end test push to this device (see
     *  bridge/internal/server/test_push.go's handleTestPushDevice and
     *  bridge/internal/relay/testpush.go's handleTestPush). Takes no
     *  request body and returns a bare `{"ok":true}` on success, like
     *  [registerDevice] -- callers only care about success/failure, so
     *  there's nothing to decode from the response body. */
    suspend fun sendTestPush() {
        post("$root/devices/test-push", "{}")
    }

    /** Retires this device's own bearer token on whichever server terminates
     *  the call (see bridge/internal/devices/devices.go's selfRevoke). The
     *  device is taken from the bearer token, so there is nothing to name in
     *  the request and nothing to decode from the reply; revoking an
     *  already-gone token is a success, not a 404. Never e2e-encrypted --
     *  see [com.sodre90.cmuxremote.data.e2e.E2eInterceptor]'s
     *  UNENCRYPTED_PATHS. */
    suspend fun selfRevoke() {
        post("$root/devices/self-revoke", "{}")
    }

    private suspend fun post(url: String, json: String) = withContext(Dispatchers.IO) {
        val request = Request.Builder()
            .url(url)
            .post(json.toRequestBody(JSON_MEDIA))
            .build()
        http.newCall(request).execute().use { resp ->
            if (!resp.isSuccessful) throw BridgeException(resp.code, resp.body?.string().orEmpty())
        }
    }

    private suspend fun postForBody(url: String, json: String): String = withContext(Dispatchers.IO) {
        val request = Request.Builder()
            .url(url)
            .post(json.toRequestBody(JSON_MEDIA))
            .build()
        http.newCall(request).execute().use { resp ->
            val body = resp.body?.string().orEmpty()
            if (!resp.isSuccessful) throw BridgeException(resp.code, body)
            body
        }
    }

    private suspend fun delete(url: String) = withContext(Dispatchers.IO) {
        val request = Request.Builder().url(url).delete().build()
        http.newCall(request).execute().use { resp ->
            if (!resp.isSuccessful) throw BridgeException(resp.code, resp.body?.string().orEmpty())
        }
    }

    private companion object {
        val JSON_MEDIA = "application/json; charset=utf-8".toMediaType()
    }
}
