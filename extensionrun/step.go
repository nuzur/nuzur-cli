package extensionrun

import (
	"errors"
	"fmt"
	"os"

	extensiongen "github.com/nuzur/extension-sdk/idl/gen"
)

// step.go is the seam that lets a caller SEE a confirmation step before answering
// it.
//
// The CLI used to answer every CONFIRMATION step with an unconditional yes and
// throw the step's payload away. For most extensions that is harmless. For sql-push
// it is the whole problem: its confirmation step carries the exact DDL it is about
// to run against a live database, and discarding that is why `deploy` could not
// tell a user what it was about to do — or notice that it was about to drop a
// table.

// StepPrompt is a CONFIRMATION step flattened into the shape a caller needs in
// order to decide on it. For sql-push, Content is the exact apply SQL that will be
// sent to the database if the step is confirmed.
type StepPrompt struct {
	StepIdentifier string `json:"step_identifier"`
	// BlockIdentifier names the display block, e.g. "sql-diff".
	BlockIdentifier string `json:"block_identifier,omitempty"`
	Title           string `json:"title,omitempty"`
	Description     string `json:"description,omitempty"`
	// ContentType is the block's content type as a name: "sql", "json", "text"…
	ContentType string `json:"content_type,omitempty"`
	Content     string `json:"content,omitempty"`
}

// StepDecision is a caller's answer to a StepPrompt. Reason is carried into the
// RunResult so the caller can explain a rejection in its own words rather than
// leaving the CLI to guess at one.
type StepDecision struct {
	Confirm bool
	Reason  string
}

// StepDecider answers every CONFIRMATION step of a run.
//
// Returning an error aborts the run WITHOUT answering the step, which leaves the
// execution blocked server-side — the same thing the pre-existing non-interactive
// path did, and the reason that behavior is expressible here at all.
type StepDecider func(StepPrompt) (StepDecision, error)

// StepOutcome records a step and how it was answered, so the payload survives the
// run instead of being consumed by the poll loop.
type StepOutcome struct {
	Prompt    StepPrompt `json:"prompt"`
	Confirmed bool       `json:"confirmed"`
	Reason    string     `json:"reason,omitempty"`
}

// DisplayBlock is a block from a terminal response — how sql-gen returns rendered
// SQL, and how a future dry-run mode would return a migration without ever raising
// a confirmation step.
type DisplayBlock struct {
	Identifier  string `json:"identifier"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Content     string `json:"content"`
}

// ErrExecutionCancelled marks a run that ended in a terminal CANCELLED status.
//
// For sql-push that is precisely what a rejected confirmation step produces, so it
// is returned ALONGSIDE a populated *RunResult: a caller that rejected on purpose
// needs the plan it rejected. Returning an error as well means every existing
// caller — none of which reject anything — keeps failing exactly as it did before.
var ErrExecutionCancelled = errors.New("extension execution was cancelled")

// stepDecider resolves the single decider a run will use.
//
// Extracted from the poll loop so the three-way precedence is testable without an
// extensions proxy. The two fallbacks are the pre-existing behavior verbatim: with
// AutoConfirmSteps the CLI says yes to everything, and without it a confirmation
// step is an error, which is what `run-extension` relies on.
func (p RunParams) stepDecider() StepDecider {
	if p.OnConfirmationStep != nil {
		return p.OnConfirmationStep
	}
	if p.AutoConfirmSteps {
		return func(s StepPrompt) (StepDecision, error) {
			fmt.Fprintf(os.Stderr, "Auto-confirming step: %s\n", s.StepIdentifier)
			return StepDecision{Confirm: true, Reason: "auto-confirmed"}, nil
		}
	}
	return func(s StepPrompt) (StepDecision, error) {
		return StepDecision{}, fmt.Errorf("extension is waiting on confirmation step %q and this run is non-interactive; enable step auto-confirmation to proceed", s.StepIdentifier)
	}
}

// stepPromptFromStep flattens a step response, tolerating a nil step and a step
// with no display block (a bare confirmation with nothing to show).
func stepPromptFromStep(s *extensiongen.ExecutionResponseTypeStepData) StepPrompt {
	if s == nil {
		return StepPrompt{}
	}
	p := StepPrompt{StepIdentifier: s.GetStepIdentifier()}
	if b := s.GetDisplayBlock(); b != nil {
		p.BlockIdentifier = b.GetIdentifier()
		p.Title = b.GetTitle()
		p.Description = b.GetDescription()
		p.ContentType = contentTypeName(b.GetContentType())
		p.Content = b.GetContent()
	}
	return p
}

// displayBlocksFrom converts terminal display blocks, skipping nils.
func displayBlocksFrom(bs []*extensiongen.ExecutionResponseDisplayBlock) []DisplayBlock {
	var out []DisplayBlock
	for _, b := range bs {
		if b == nil {
			continue
		}
		out = append(out, DisplayBlock{
			Identifier:  b.GetIdentifier(),
			Title:       b.GetTitle(),
			Description: b.GetDescription(),
			ContentType: contentTypeName(b.GetContentType()),
			Content:     b.GetContent(),
		})
	}
	return out
}

// contentTypeName renders a content type as a stable lowercase name for JSON
// output. An unrecognized type falls back to its numeric form rather than being
// silently reported as plain text.
func contentTypeName(t extensiongen.DisplayBlockContentType) string {
	switch t {
	case extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_PLAIN_TEXT:
		return "text"
	case extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_JSON:
		return "json"
	case extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_SQL:
		return "sql"
	case extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_PROTO:
		return "proto"
	case extensiongen.DisplayBlockContentType_DISPLAY_BLOCK_CONTENT_TYPE_GOLANG:
		return "golang"
	default:
		return fmt.Sprintf("unknown(%d)", int32(t))
	}
}

// SQLPreview returns the SQL this run was shown, or "".
//
// It prefers a terminal display block over a step's so callers do not care which
// channel the SQL arrived on. That indifference is deliberate: today sql-push can
// only surface a migration by raising a confirmation step, but if it ever gains a
// real dry-run mode it will return the same SQL as a terminal block, and this is
// the only line that would need to know.
func (r *RunResult) SQLPreview() string {
	if r == nil {
		return ""
	}
	for _, b := range r.DisplayBlocks {
		if b.ContentType == "sql" && b.Content != "" {
			return b.Content
		}
	}
	for _, s := range r.Steps {
		if s.Prompt.Content != "" {
			return s.Prompt.Content
		}
	}
	return ""
}

// DisplayBlock returns the terminal block with the given identifier, or nil.
func (r *RunResult) DisplayBlock(identifier string) *DisplayBlock {
	if r == nil {
		return nil
	}
	for idx := range r.DisplayBlocks {
		if r.DisplayBlocks[idx].Identifier == identifier {
			return &r.DisplayBlocks[idx]
		}
	}
	return nil
}
