package wire

// The kinds of blocking agent prompt a feed item can be, as the app names
// them in POST /feed/{id}/reply (FeedReply.Kind) and reads them in a pending
// item's "kind". Mirrored in android's Dtos.kt.
const (
	FeedKindPermissionRequest = "permissionRequest"
	FeedKindQuestion          = "question"
	FeedKindExitPlan          = "exitPlan"
)

// KnownFeedKind reports whether kind is one the wire protocol defines.
func KnownFeedKind(kind string) bool {
	switch kind {
	case FeedKindPermissionRequest, FeedKindQuestion, FeedKindExitPlan:
		return true
	}
	return false
}
