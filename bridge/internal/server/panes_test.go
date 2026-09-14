package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/host/cmuxhost"
	"github.com/sodre90/term-bridge/internal/wire"
)

func TestSplitFromTheViewedSurface(t *testing.T) {
	for _, direction := range []string{"left", "right", "up", "down"} {
		t.Run(direction, func(t *testing.T) {
			srv, tok, logPath := layoutTestServer(t)
			resp, body := do(t, "POST", srv.URL+"/sessions/"+wsID+"/panes", tok,
				`{"surface_id":"`+surfID+`","placement":"`+direction+`"}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
			}
			var created wire.CreatePaneResponse
			if err := json.Unmarshal(body, &created); err != nil || created.SurfaceID != newSurf || created.PaneID != newPane {
				t.Fatalf("reply %s (%v)", body, err)
			}
			calls := rpcCalls(t, logPath)
			assertExplicitTargets(t, calls)
			if len(calls) != 1 || calls[0].Method != "surface.split" {
				t.Fatalf("want one surface.split, got %+v", calls)
			}
			p := calls[0].Params
			if p["surface_id"] != surfID || p["direction"] != direction || p["focus"] != false {
				t.Fatalf("params %v", p)
			}
		})
	}
}

// A tab needs the pane that holds the viewed surface, which only pane.list
// knows; the bridge looks it up rather than letting cmux pick the focused
// pane on the Mac.
func TestTabGoesIntoThePaneHoldingTheViewedSurface(t *testing.T) {
	srv, tok, logPath := layoutTestServer(t)
	resp, body := do(t, "POST", srv.URL+"/sessions/"+wsID+"/panes", tok,
		`{"surface_id":"`+surfID2+`","placement":"tab"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	var created wire.CreatePaneResponse
	if err := json.Unmarshal(body, &created); err != nil || created.SurfaceID != newSurf || created.PaneID != paneID2 {
		t.Fatalf("reply %s (%v)", body, err)
	}
	calls := rpcCalls(t, logPath)
	assertExplicitTargets(t, calls)
	if len(calls) != 2 || calls[0].Method != "pane.list" || calls[1].Method != "surface.create" {
		t.Fatalf("want pane.list then surface.create, got %+v", calls)
	}
	p := calls[1].Params
	if p["workspace_id"] != wsID || p["pane_id"] != paneID2 || p["type"] != "terminal" || p["focus"] != false {
		t.Fatalf("params %v", p)
	}
}

func TestTabForASurfaceNotInTheWorkspaceIs404WithoutCreating(t *testing.T) {
	srv, tok, logPath := layoutTestServer(t)
	resp, _ := do(t, "POST", srv.URL+"/sessions/"+wsID+"/panes", tok,
		`{"surface_id":"`+newSurf+`","placement":"tab"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	calls := rpcCalls(t, logPath)
	if len(calls) != 1 || calls[0].Method != "pane.list" {
		t.Fatalf("only pane.list may run; got %+v", calls)
	}
}

func TestUnknownPlacementIs400BeforeCallingCmux(t *testing.T) {
	srv, tok, logPath := layoutTestServer(t)
	for _, placement := range []string{"", "diagonal", "TAB", "Right"} {
		resp, _ := do(t, "POST", srv.URL+"/sessions/"+wsID+"/panes", tok,
			`{"surface_id":"`+surfID+`","placement":"`+placement+`"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("placement %q: want 400, got %d", placement, resp.StatusCode)
		}
	}
	if calls := rpcCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("cmux must not be called; got %+v", calls)
	}
}

func TestCreatePaneCmuxFailure502(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, "#!/bin/sh\nexit 1\n")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := do(t, "POST", srv.URL+"/sessions/"+wsID+"/panes", tok,
		`{"surface_id":"`+surfID+`","placement":"right"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}

func TestCloseSurfaceNamesTheSurface(t *testing.T) {
	srv, tok, logPath := layoutTestServer(t)
	resp, _ := do(t, "DELETE", srv.URL+"/sessions/"+wsID+"/panes/"+surfID2, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	calls := rpcCalls(t, logPath)
	assertExplicitTargets(t, calls)
	if len(calls) != 1 || calls[0].Method != "surface.close" || calls[0].Params["surface_id"] != surfID2 {
		t.Fatalf("got %+v", calls)
	}
}

func TestLayoutDropsTheSidebarOffsetAndKeepsProportions(t *testing.T) {
	srv, tok, _ := layoutTestServer(t)
	resp, body := do(t, "GET", srv.URL+"/sessions/"+wsID+"/layout", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	var layout wire.Layout
	if err := json.Unmarshal(body, &layout); err != nil {
		t.Fatal(err)
	}
	if layout.Estimated || len(layout.Panes) != 2 {
		t.Fatalf("layout %+v", layout)
	}
	left, right := layout.Panes[0], layout.Panes[1]
	if left.ID != paneID || !left.Focused || left.SelectedSurfaceID != surfID || strings.Join(left.SurfaceIDs, ",") != surfID {
		t.Fatalf("left %+v", left)
	}
	if left.X != 0 || left.Y != 0 || left.W != 0.5 || left.H != 1 {
		t.Fatalf("left rect %+v", left)
	}
	if right.X != 0.5 || right.Y != 0 || right.W != 0.5 || right.H != 1 || right.Focused {
		t.Fatalf("right rect %+v", right)
	}
}

func TestNormaliseLayoutOfATwoByTwoSplit(t *testing.T) {
	// The live shape after splitting the right column downward.
	panes, err := cmuxhost.ParsePanes([]byte(`{"panes":[
	 {"id":"a","pixel_frame":{"height":1382,"width":1156,"x":248,"y":28}},
	 {"id":"b","pixel_frame":{"height":691,"width":1156,"x":1404,"y":28}},
	 {"id":"c","pixel_frame":{"height":691,"width":1156,"x":1404,"y":719}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	layout := normaliseLayout(panes)
	want := []wire.LayoutPane{
		{ID: "a", X: 0, Y: 0, W: 0.5, H: 1, SurfaceIDs: []string{}},
		{ID: "b", X: 0.5, Y: 0, W: 0.5, H: 0.5, SurfaceIDs: []string{}},
		{ID: "c", X: 0.5, Y: 0.5, W: 0.5, H: 0.5, SurfaceIDs: []string{}},
	}
	for i, p := range layout.Panes {
		if p.ID != want[i].ID || p.X != want[i].X || p.Y != want[i].Y || p.W != want[i].W || p.H != want[i].H {
			t.Fatalf("pane %d: got %+v want %+v", i, p, want[i])
		}
	}
}

// A workspace never shown on the Mac reports every frame as zero; the app
// gets equal columns in index order and a flag saying so.
func TestNormaliseLayoutWithoutGeometryIsEstimatedColumns(t *testing.T) {
	panes, err := cmuxhost.ParsePanes([]byte(`{"panes":[
	 {"id":"a","focused":true,"pixel_frame":{"height":0,"width":0,"x":0,"y":0},"surface_ids":["s1"],"selected_surface_id":"s1"},
	 {"id":"b","pixel_frame":{"height":0,"width":0,"x":0,"y":0},"surface_ids":["s2","s3"],"selected_surface_id":"s3"},
	 {"id":"c","pixel_frame":{"height":0,"width":0,"x":0,"y":0}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	layout := normaliseLayout(panes)
	if !layout.Estimated {
		t.Fatal("want estimated")
	}
	third := 1.0 / 3
	for i, p := range layout.Panes {
		if p.X != float64(i)*third || p.Y != 0 || p.W != third || p.H != 1 {
			t.Fatalf("pane %d rect %+v", i, p)
		}
	}
	if layout.Panes[1].SelectedSurfaceID != "s3" || len(layout.Panes[1].SurfaceIDs) != 2 || layout.Panes[2].SurfaceIDs == nil {
		t.Fatalf("surface fields %+v", layout.Panes)
	}
}

func TestNormaliseLayoutOfNoPanes(t *testing.T) {
	layout := normaliseLayout(nil)
	if layout.Estimated || layout.Panes == nil || len(layout.Panes) != 0 {
		t.Fatalf("layout %+v", layout)
	}
}
