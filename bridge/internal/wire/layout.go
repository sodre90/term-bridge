package wire

// Placement says where a new pane goes relative to the surface the app is
// viewing: one of the four split directions cmux's surface.split takes, or
// a tab in the same pane. Mirrored in Kotlin as PanePlacement.
const (
	PlacementLeft  = "left"
	PlacementRight = "right"
	PlacementUp    = "up"
	PlacementDown  = "down"
	PlacementTab   = "tab"
)

// CreateWorkspaceRequest is the body of POST /sessions. Title may be empty,
// in which case cmux derives one.
type CreateWorkspaceRequest struct {
	CWD   string `json:"cwd"`
	Title string `json:"title,omitempty"`
}

// CreateWorkspaceResponse names what cmux made: the workspace and its first
// terminal surface, which the app opens straight away.
type CreateWorkspaceResponse struct {
	WorkspaceID string `json:"workspace_id"`
	SurfaceID   string `json:"surface_id"`
}

// CreatePaneRequest is the body of POST /sessions/{id}/panes. SurfaceID is
// the surface the app is viewing; Placement is one of the Placement* values.
type CreatePaneRequest struct {
	SurfaceID string `json:"surface_id"`
	Placement string `json:"placement"`
}

// CreatePaneResponse names the new terminal surface and the pane holding it.
type CreatePaneResponse struct {
	SurfaceID string `json:"surface_id"`
	PaneID    string `json:"pane_id"`
}

// SelectRequest is the body of POST /sessions/{id}/select. SurfaceID, when
// set, is focused after the workspace is selected on the Mac.
type SelectRequest struct {
	SurfaceID string `json:"surface_id,omitempty"`
}

// Layout is GET /sessions/{id}/layout: where each pane of a workspace sits,
// as fractions of the panes' own bounding box so the app never sees pixels
// or cmux's sidebar offset. Estimated is true when cmux had no geometry yet
// (a workspace never shown on the Mac) and the panes were laid out as equal
// columns in index order instead.
type Layout struct {
	Estimated bool         `json:"estimated"`
	Panes     []LayoutPane `json:"panes"`
}

// LayoutPane is one pane's place in a Layout. X, Y, W and H are in [0, 1].
type LayoutPane struct {
	ID                string   `json:"id"`
	X                 float64  `json:"x"`
	Y                 float64  `json:"y"`
	W                 float64  `json:"w"`
	H                 float64  `json:"h"`
	Focused           bool     `json:"focused"`
	SurfaceIDs        []string `json:"surface_ids"`
	SelectedSurfaceID string   `json:"selected_surface_id"`
}
