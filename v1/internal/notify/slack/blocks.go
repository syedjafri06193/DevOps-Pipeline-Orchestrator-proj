package slack

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/syedjafri06193/orch/internal/engine"
	"github.com/syedjafri06193/orch/internal/store"
)

// Message is what gets posted or updated.
type Message struct {
	Channel string  `json:"channel"`
	Text    string  `json:"text"` // fallback for notifications and screen readers
	Blocks  []Block `json:"blocks,omitempty"`
	// TS identifies an existing message to update rather than replace.
	TS string `json:"ts,omitempty"`
	// ThreadTS posts into a thread, used for the terminal-failure message so
	// it appears under the deploy it belongs to.
	ThreadTS string `json:"thread_ts,omitempty"`
}

// Block is a Block Kit block, kept as a loose map because the schema is
// Slack's and pinning it in Go types buys nothing here.
type Block map[string]any

func section(text string) Block {
	return Block{"type": "section", "text": Block{"type": "mrkdwn", "text": text}}
}

func fields(pairs ...string) Block {
	var fs []Block
	for i := 0; i+1 < len(pairs); i += 2 {
		fs = append(fs, Block{"type": "mrkdwn", "text": "*" + pairs[i] + "*\n" + pairs[i+1]})
	}
	return Block{"type": "section", "fields": fs}
}

func context_(text string) Block {
	return Block{"type": "context", "elements": []Block{{"type": "mrkdwn", "text": text}}}
}

func divider() Block { return Block{"type": "divider"} }

func actions(buttons ...Block) Block {
	return Block{"type": "actions", "elements": buttons}
}

func button(text, actionID, value, style string) Block {
	b := Block{
		"type":      "button",
		"text":      Block{"type": "plain_text", "text": text},
		"action_id": actionID,
		"value":     value,
	}
	if style != "" {
		b["style"] = style
	}
	return b
}

// ActionIDs the interaction handler dispatches on.
const (
	ActionApprove = "orch_approve"
	ActionReject  = "orch_reject"
	ActionAbort   = "orch_abort"
)

// DeployMessage renders the card from section 10.4.
//
//	"Update one message in place rather than posting a stream. A deploy that
//	posts eight messages to #deploys trains people to mute the channel."
//
// So this builds the whole current state of a deployment every time, and the
// caller sends it with chat.update against the stored ts.
func DeployMessage(channel string, ev engine.Event, samples []*store.HealthSample, dashboardURL string) Message {
	icon := stateIcon(ev.State)
	title := fmt.Sprintf("%s  *%s* → *%s*", icon, ev.App, ev.Environment)

	blocks := []Block{
		section(title),
		fields(
			"Version", "`"+ev.Version+"`",
			"From", "`"+orDash(ev.Previous)+"`",
			"By", "@"+orDash(ev.Actor),
			"Strategy", orDash(ev.Strategy),
		),
	}

	if bar := progressBar(ev.State); bar != "" {
		blocks = append(blocks, section(bar))
	}

	if len(samples) > 0 {
		blocks = append(blocks, section(renderSamples(samples)))
	}

	if ev.Reason != "" {
		blocks = append(blocks, section(":memo: "+ev.Reason))
	}

	for k, v := range ev.Detail {
		if k == "action" {
			blocks = append(blocks, section(":rotating_light: *"+v+"*"))
		}
	}

	var buttons []Block
	if dashboardURL != "" {
		buttons = append(buttons, Block{
			"type":      "button",
			"text":      Block{"type": "plain_text", "text": "View dashboard"},
			"url":       dashboardURL + "/deployments/" + ev.DeploymentID,
			"action_id": "orch_view",
		})
	}
	switch ev.State {
	case store.StateAwaitApproval:
		buttons = append(buttons,
			button("Approve", ActionApprove, ev.DeploymentID, "primary"),
			button("Reject", ActionReject, ev.DeploymentID, "danger"))
	case store.StateDeploying, store.StateVerifying, store.StatePromoting:
		buttons = append(buttons, button("Abort", ActionAbort, ev.DeploymentID, "danger"))
	}
	if len(buttons) > 0 {
		blocks = append(blocks, actions(buttons...))
	}

	blocks = append(blocks, context_(fmt.Sprintf("`%s` · %s",
		ev.DeploymentID, ev.At.Format(time.RFC3339))))

	return Message{
		Channel: channel,
		// The fallback text matters: it is what appears in a phone
		// notification and in a screen reader, and Block Kit messages without
		// it show as "This content can't be displayed".
		Text:   fmt.Sprintf("%s %s → %s: %s", icon, ev.App, ev.Environment, ev.State),
		Blocks: blocks,
	}
}

// FailureMessage is posted as a NEW message rather than an update.
//
// Section 10.4: "Post a *new* message only for terminal failure states, so
// failures break through the noise." An update to a message people have
// scrolled past is a failure nobody sees.
func FailureMessage(channel string, ev engine.Event, mentions []string) Message {
	var b strings.Builder
	fmt.Fprintf(&b, "%s *%s → %s %s*\n", stateIcon(ev.State), ev.App, ev.Environment, ev.State)
	fmt.Fprintf(&b, "Version `%s`", ev.Version)
	if ev.Previous != "" {
		fmt.Fprintf(&b, " (was `%s`)", ev.Previous)
	}
	if ev.Reason != "" {
		fmt.Fprintf(&b, "\n%s", ev.Reason)
	}
	if action, ok := ev.Detail["action"]; ok {
		fmt.Fprintf(&b, "\n\n:rotating_light: *%s*", action)
	}
	if refusal, ok := ev.Detail["rollback_refused"]; ok {
		fmt.Fprintf(&b, "\n\n*Rollback was refused:*\n```%s```", refusal)
	}
	if len(mentions) > 0 {
		fmt.Fprintf(&b, "\n\n%s", strings.Join(mentions, " "))
	}

	return Message{
		Channel: channel,
		Text:    fmt.Sprintf("%s %s → %s %s", stateIcon(ev.State), ev.App, ev.Environment, ev.State),
		Blocks:  []Block{section(b.String()), divider()},
	}
}

// ApprovedBlocks replaces the buttons once someone has acted, so the message
// cannot be clicked twice and shows who decided.
func ApprovedBlocks(ev engine.Event, user string, approved bool) []Block {
	verb := "approved"
	icon := ":white_check_mark:"
	if !approved {
		verb, icon = "rejected", ":x:"
	}
	return []Block{
		section(fmt.Sprintf("%s  *%s* → *%s*", stateIcon(ev.State), ev.App, ev.Environment)),
		fields("Version", "`"+ev.Version+"`", "Requested by", "@"+ev.Actor),
		context_(fmt.Sprintf("%s %s by @%s at %s", icon, verb, user,
			time.Now().UTC().Format(time.RFC3339))),
	}
}

func stateIcon(s store.State) string {
	switch s {
	case store.StatePending, store.StatePreflight:
		return ":hourglass_flowing_sand:"
	case store.StateAwaitApproval:
		return ":raised_hand:"
	case store.StateDeploying:
		return ":rocket:"
	case store.StateVerifying:
		return ":mag:"
	case store.StatePromoting:
		return ":arrow_up:"
	case store.StateSucceeded:
		return ":white_check_mark:"
	case store.StateRollingBack:
		return ":leftwards_arrow_with_hook:"
	case store.StateRolledBack:
		return ":leftwards_arrow_with_hook:"
	case store.StateRollbackFailed, store.StateUnknown:
		return ":rotating_light:"
	case store.StateFailed:
		return ":x:"
	case store.StateAborted:
		return ":black_square_for_stop:"
	}
	return ":grey_question:"
}

func progressBar(s store.State) string {
	steps := []store.State{
		store.StatePreflight, store.StateDeploying, store.StateVerifying,
		store.StatePromoting, store.StateSucceeded,
	}
	idx := -1
	for i, st := range steps {
		if st == s {
			idx = i
		}
	}
	if idx < 0 {
		return ""
	}
	const width = 20
	filled := (idx + 1) * width / len(steps)
	return fmt.Sprintf("`%s%s`  %d%%  %s",
		strings.Repeat("▓", filled), strings.Repeat("░", width-filled),
		(idx+1)*100/len(steps), strings.ToLower(string(s)))
}

// renderSamples shows the most recent observation per criterion.
//
// One line per criterion, not one per sample: a ten-minute bake at a
// thirty-second interval produces twenty samples per criterion, and putting
// them all in a Slack message is how the channel gets muted.
func renderSamples(samples []*store.HealthSample) string {
	latest := map[string]*store.HealthSample{}
	var order []string
	for _, s := range samples {
		if _, seen := latest[s.Criterion]; !seen {
			order = append(order, s.Criterion)
		}
		latest[s.Criterion] = s
	}
	var b strings.Builder
	for _, name := range order {
		s := latest[name]
		mark := ":white_check_mark:"
		if !s.Healthy {
			mark = ":x:"
		}
		fmt.Fprintf(&b, "%s  *%s*  %.4g", mark, name, s.Value)
		if s.Baseline > 0 {
			fmt.Fprintf(&b, "  (baseline %.4g)", s.Baseline)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// MarshalMessage renders a message as the JSON Slack expects.
func MarshalMessage(m Message) ([]byte, error) { return json.Marshal(m) }
