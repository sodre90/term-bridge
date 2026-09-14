package com.sodre90.cmuxremote.model

import com.sodre90.cmuxremote.ui.inbox.isPendingInboxKind
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * The bridge counts the pending prompts the Inbox would list and sends the
 * number on `GET /sessions` as `pending_count`, so the badge costs no
 * `/feed/pending` fetch. The bridge's count and this side's own count of the
 * same items must agree, or the badge and the Inbox drift apart.
 *
 * The items are copied from the Go side's pendingCountFixtureItems
 * (internal/server/sessions_test.go); TestSessionsCountsThePromptsTheInboxWouldList
 * pins the bridge to 2 for them.
 */
class PendingCountFixtureTest {

    private val fixtureItems =
        """{"request_id":"q1","kind":"question","cwd":"/tmp/proj"},""" +
            """{"request_id":"p1","kind":"permissionRequest","cwd":"/tmp/proj"},""" +
            """{"request_id":"e1","kind":"exitPlan","cwd":"/tmp/proj"},""" +
            """{"request_id":"t1","kind":"toolUse","cwd":"/tmp/proj"}"""

    private val bridgeCount = 2

    @Test
    fun theBridgeCountsExactlyTheItemsTheInboxWouldList() {
        val items = BridgeJson.decodeFromString(
            PendingFeedResponse.serializer(),
            """{"items":[$fixtureItems]}""",
        ).items
        assertEquals(bridgeCount, items.count { isPendingInboxKind(it.kind) })
    }

    @Test
    fun theCountRidesOnTheSessionsEnvelope() {
        val response = BridgeJson.decodeFromString(
            WorkspacesResponse.serializer(),
            """{"workspaces":[],"host":{"name":"mac","kind":"cmux","capabilities":{"tabs":true,"feed":true}},""" +
                """"pending_count":$bridgeCount}""",
        )
        assertEquals(bridgeCount, response.pendingCount)
    }

    @Test
    fun anOlderBridgeLeavesTheCountUnknown() {
        val response = BridgeJson.decodeFromString(WorkspacesResponse.serializer(), """{"workspaces":[]}""")
        assertNull(response.pendingCount)
    }
}
