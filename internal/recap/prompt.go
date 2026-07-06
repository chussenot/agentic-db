package recap

import "strings"

// promptPreamble is shared by every period: it tells the model what it is
// receiving (a recap digest arrives on stdin, not in this instruction) and how
// to behave. The per-period suffix sets the shape and altitude of the output.
const promptPreamble = `You are turning a machine-generated recap of my Claude Code sessions into a report I can read out or paste into a status update.

The recap digest arrives on stdin as markdown: per-project sessions with a topic, git branch, active time, turn/prompt counts, my opening ask, and focus streaks. It is derived from a local activity log — "active time" is real working time (idle waits excluded), "prompts" are permission requests I answered, "turns" are places Claude paused for me.

Rules:
- Write in the first person ("I …"), past tense, plain and concrete.
- Use "## " markdown headers for sections — never bold (**…**) as a header.
- Ground every claim in the digest; never invent work, outcomes, or numbers that aren't there.
- The opening ask states intent, not necessarily result — say "worked on"/"looked into", not "shipped", unless the digest clearly implies completion.
- Skip trivial or aborted sessions; group related work rather than listing every session.
- Do NOT add a metrics, stats, or totals section — exact figures are appended separately.
- No preamble, no "here is your report" — just the report.
`

const promptDay = `
Produce a DAILY standup, terse. The window is usually the previous day but can
span a weekend (a Monday report covers Fri–Sun) — see the digest's date range
and say "over the weekend" rather than "yesterday" when it spans more than a day.
## Done / progressed — 3–6 bullets of what I actually worked on in this window.
## In flight — anything clearly mid-stream.
## Blockers / waiting — only if the digest suggests it (e.g. long unanswered prompts). Omit the section if none.
Keep it under ~150 words.`

const promptWeek = `
Produce a WEEKLY summary:
- Lead with 2–4 themes that group the week's work (by project or thread), each with a one-line "what and why".
- Then a short "highlights" list of the most substantial pieces.
- Close with one line on where focus went (which projects dominated active time).
Aim for ~250 words; prioritise signal over completeness.`

const promptQuarter = `
Produce a QUARTERLY narrative:
- Open with a 2–3 sentence arc of what this period was about.
- Then the major threads (3–6), each a short paragraph: the goal, what moved, and rough scale (active time / number of sessions).
- Note any long-running or recurring efforts.
Write it as prose a manager or my future self could skim; omit day-level detail.`

// Prompt returns the recap-prompt text for a period ("day"/"week"/"quarter").
// Unknown periods fall back to the daily template. A non-empty note is appended
// as a revision directive — used to iterate on a weak recap: the reader's
// feedback steers a fresh generation without changing the base templates.
func Prompt(period, note string) string {
	var base string
	switch period {
	case "week":
		base = promptPreamble + promptWeek
	case "quarter":
		base = promptPreamble + promptQuarter
	default:
		base = promptPreamble + promptDay
	}
	if strings.TrimSpace(note) != "" {
		base += "\n\nREVISION: a previous draft of this report was reviewed and found" +
			" lacking. Produce a fresh report that addresses this feedback, while still" +
			" following every rule above:\n" + strings.TrimSpace(note)
	}
	return base
}
