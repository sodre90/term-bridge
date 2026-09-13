package com.sodre90.cmuxremote.data

import com.sodre90.cmuxremote.model.FeedReply
import com.sodre90.cmuxremote.model.PanePlacement
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test

class BridgeClientTest {

    private lateinit var server: MockWebServer
    private lateinit var client: BridgeClient

    @Before
    fun setUp() {
        server = MockWebServer().apply { start() }
        val http = OkHttpClient.Builder()
            .addInterceptor(BearerInterceptor("tok-7"))
            .build()
        client = BridgeClient(http, server.url("/").toString())
    }

    @After
    fun tearDown() {
        server.shutdown()
    }

    @Test
    fun sessionsDecodesEnvelopeAndSendsBearer() {
        server.enqueue(
            MockResponse().setBody(
                """{"workspaces":[{"id":"a","cwd":"/x","title":"build","preview":"Claude is waiting","has_unread":true,
                "terminals":[{"id":"t-a","cwd":"/x","title":"build","focused":true,"ready":true,"kind":"agent"}]}]}""",
            ),
        )

        val list = runBlocking { client.sessions() }

        assertEquals(1, list.size)
        assertEquals("a", list[0].id)
        assertTrue(list[0].hasUnread)
        assertEquals(1, list[0].terminals.size)
        assertEquals("t-a", list[0].terminals[0].id)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertEquals("/sessions", req.path)
        assertEquals("Bearer tok-7", req.getHeader("Authorization"))
    }

    @Test
    fun replyFeedPostsToFeedPathWithBody() {
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))

        val reply = FeedReply(
            kind = "permissionRequest",
            requestId = "req-1",
            params = buildJsonObject { put("decision", "approve") },
        )
        runBlocking { client.replyFeed("feed-9", reply) }

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/feed/feed-9/reply", req.path)
        val body = req.body.readUtf8()
        assertTrue(body.contains("\"request_id\":\"req-1\""))
        assertTrue(body.contains("approve"))
    }

    @Test
    fun sessionsDecodesYoloMode() {
        server.enqueue(
            MockResponse().setBody(
                """{"workspaces":[{"id":"a","cwd":"/x","title":"build","yolo_mode":"bypass","terminals":[]}]}""",
            ),
        )

        val list = runBlocking { client.sessions() }

        assertEquals("bypass", list[0].yoloMode)
    }

    @Test
    fun setYoloModePostsToYoloModePath() {
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))

        runBlocking { client.setYoloMode("ws-1", "bypass") }

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/sessions/ws-1/yolo-mode", req.path)
        assertTrue(req.body.readUtf8().contains("\"mode\":\"bypass\""))
    }

    @Test
    fun registerDevicePostsFcmToken() {
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))

        runBlocking { client.registerDevice("fcm-xyz") }

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/devices/register", req.path)
        assertTrue(req.body.readUtf8().contains("\"fcm_token\":\"fcm-xyz\""))
    }

    @Test
    fun selfRevokeNamesNoDeviceAndSendsThisDevicesToken() {
        server.enqueue(MockResponse().setBody("{}"))

        runBlocking { client.selfRevoke() }

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/devices/self-revoke", req.path)
        assertEquals("Bearer tok-7", req.getHeader("Authorization"))
        assertEquals("{}", req.body.readUtf8())
    }

    @Test
    fun createWorkspacePostsTheDirectoryAndReadsBackWhatWasMade() {
        server.enqueue(
            MockResponse().setBody(
                """{"workspace_id":"21B6A522-37FB-46A8-ABB3-F69917522BEF",
                    "surface_id":"2620FAA3-A6AE-41AD-8C6A-4A4ED139D622"}""",
            ),
        )

        val created = runBlocking { client.createWorkspace("/Users/me/prj/thing", title = "  ") }

        assertEquals("21B6A522-37FB-46A8-ABB3-F69917522BEF", created.workspaceId)
        assertEquals("2620FAA3-A6AE-41AD-8C6A-4A4ED139D622", created.surfaceId)
        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/sessions", req.path)
        // A blank title is left out so cmux picks one.
        assertEquals("""{"cwd":"/Users/me/prj/thing"}""", req.body.readUtf8())
    }

    @Test
    fun createPanePostsThePlacementUnderTheWorkspace() {
        server.enqueue(MockResponse().setBody("""{"surface_id":"new-s","pane_id":"new-p"}"""))

        val created = runBlocking { client.createPane("ws-1", "surf-1", PanePlacement.DOWN) }

        assertEquals("new-s", created.surfaceId)
        assertEquals("new-p", created.paneId)
        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/sessions/ws-1/panes", req.path)
        assertEquals("""{"surface_id":"surf-1","placement":"down"}""", req.body.readUtf8())
    }

    @Test
    fun selectWorkspaceNamesTheSurfaceOnlyWhenGiven() {
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))

        runBlocking {
            client.selectWorkspace("ws-1")
            client.selectWorkspace("ws-1", "surf-2")
        }

        val bare = server.takeRequest()
        assertEquals("/sessions/ws-1/select", bare.path)
        assertEquals("{}", bare.body.readUtf8())
        assertEquals("""{"surface_id":"surf-2"}""", server.takeRequest().body.readUtf8())
    }

    @Test
    fun closesAreDeletes() {
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))

        runBlocking {
            client.closeWorkspace("ws-1")
            client.closeSurface("ws-1", "surf-2")
        }

        val ws = server.takeRequest()
        assertEquals("DELETE", ws.method)
        assertEquals("/sessions/ws-1", ws.path)
        val surface = server.takeRequest()
        assertEquals("DELETE", surface.method)
        assertEquals("/sessions/ws-1/panes/surf-2", surface.path)
    }

    @Test
    fun layoutDecodesPanesAndTheEstimatedFlag() {
        server.enqueue(
            MockResponse().setBody(
                """{"estimated":true,"panes":[{"id":"p1","x":0,"y":0,"w":0.5,"h":1,"focused":true,
                    "surface_ids":["s1"],"selected_surface_id":"s1"}]}""",
            ),
        )

        val layout = runBlocking { client.layout("ws-1") }

        assertTrue(layout.estimated)
        assertEquals(0.5, layout.panes.single().w, 0.0)
        assertEquals("/sessions/ws-1/layout", server.takeRequest().path)
    }

    @Test
    fun nonSuccessThrowsBridgeException() {
        server.enqueue(MockResponse().setResponseCode(502).setBody("""{"error":"cmux unavailable"}"""))

        try {
            runBlocking { client.sessions() }
            fail("expected BridgeException")
        } catch (e: BridgeException) {
            assertEquals(502, e.code)
            assertTrue(e.bodyText.contains("cmux unavailable"))
        }
    }
}
