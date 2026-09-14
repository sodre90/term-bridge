package agentfeed

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sodre90/term-bridge/internal/wire"
)

// wireItem is one pending prompt in the shape cmux's feed.list gives the
// app (android's Dtos.kt PendingFeedItem), so the phone renders and
// answers a tmux prompt with the code it already has. tool_input is a JSON
// string holding the tool's JSON, as cmux sends it.
type wireItem struct {
	ID                  string         `json:"id"`
	RequestID           string         `json:"request_id"`
	Kind                string         `json:"kind"`
	Status              string         `json:"status"`
	Title               string         `json:"title"`
	CWD                 string         `json:"cwd"`
	WorkstreamID        string         `json:"workstream_id"`
	CreatedAt           string         `json:"created_at"`
	ToolName            string         `json:"tool_name,omitempty"`
	ToolInput           string         `json:"tool_input,omitempty"`
	QuestionPrompt      string         `json:"question_prompt,omitempty"`
	QuestionMultiSelect bool           `json:"question_multi_select"`
	Questions           []wireQuestion `json:"questions,omitempty"`
}

type wireQuestion struct {
	ID          string       `json:"id"`
	Header      string       `json:"header"`
	Prompt      string       `json:"prompt"`
	MultiSelect bool         `json:"multi_select"`
	Options     []wireOption `json:"options"`
}

type wireOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// askUserQuestionInput is AskUserQuestion's tool_input as the hook carries
// it (verified 2026-09-14).
type askUserQuestionInput struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

func toWire(it *item) wireItem {
	w := wireItem{
		ID:           it.id,
		RequestID:    it.id,
		Kind:         it.kind,
		Status:       "pending",
		Title:        filepath.Base(it.cwd),
		CWD:          it.cwd,
		WorkstreamID: it.sessionID,
		CreatedAt:    it.createdAt.UTC().Format(time.RFC3339Nano),
		ToolName:     it.toolName,
	}
	if len(it.toolInput) > 0 {
		w.ToolInput = string(it.toolInput)
	}
	if it.kind == wire.FeedKindQuestion {
		w.Questions = questionsOf(it.toolInput)
		if len(w.Questions) > 0 {
			w.QuestionPrompt = w.Questions[0].Prompt
			w.QuestionMultiSelect = w.Questions[0].MultiSelect
		}
	}
	return w
}

func questionsOf(toolInput json.RawMessage) []wireQuestion {
	var in askUserQuestionInput
	if err := json.Unmarshal(toolInput, &in); err != nil {
		return nil
	}
	out := make([]wireQuestion, 0, len(in.Questions))
	for i, q := range in.Questions {
		wq := wireQuestion{
			ID:          fmt.Sprintf("q%d", i),
			Header:      q.Header,
			Prompt:      q.Question,
			MultiSelect: q.MultiSelect,
			Options:     make([]wireOption, 0, len(q.Options)),
		}
		for j, o := range q.Options {
			wq.Options = append(wq.Options, wireOption{ID: fmt.Sprintf("q%d-o%d", i, j), Label: o.Label, Description: o.Description})
		}
		out = append(out, wq)
	}
	return out
}
