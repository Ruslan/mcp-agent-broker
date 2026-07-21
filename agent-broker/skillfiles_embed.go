package main

import (
	_ "embed"
	"fmt"
	"strings"
)

// BrokerSkillVersion is the single source of truth for the broker-async-poll
// skill's script version. Bump it (+1) whenever broker-poll.sh or await-poll.sh
// changes in a way an already-installed copy elsewhere should pick up, then
// update the "# BROKER_SKILL_VERSION=<n>" marker in both scripts to match and
// run `make sync-skillfiles`. TestBrokerSkillVersionMarkersMatchConstant
// enforces that the markers equal this constant; skill-install advertises this
// number so an agent can compare it against an already-installed skill.
const BrokerSkillVersion = 5

// The embedded sources below are the CANONICAL broker-async-poll skill files —
// this is where a developer edits the scripts and SKILL.md. They live under
// this package (skillfiles/) so `//go:embed` can reach them directly (it cannot
// climb above the package dir with ".."). .claude/skills/broker-async-poll/ is a
// GENERATED install copy, produced from this canon by `make sync-skillfiles`;
// TestSkillfilesMatchCanonical asserts the two stay byte-identical.
//
//go:embed skillfiles/broker-poll.sh
var skillBrokerPollSH string

//go:embed skillfiles/await-poll.sh
var skillAwaitPollSH string

//go:embed skillfiles/broker-monitor.sh
var skillBrokerMonitorSH string

//go:embed skillfiles/SKILL.md
var skillMD string

const skillInstallPromptName = "skill-install"

const skillInstallPromptDescription = "Install the broker-async-poll skill (broker-poll.sh/await-poll.sh) into this project so async, keep-working task orchestration against this broker works here."

const skillInstallTargetDir = ".claude/skills/broker-async-poll"

// skillInstallFile pairs one embedded file's bytes with its filename and the
// fenced-code-block language tag, in the order they should be presented/written.
type skillInstallFile struct {
	name string
	lang string
	body string
}

func skillInstallFiles() []skillInstallFile {
	return []skillInstallFile{
		{"SKILL.md", "markdown", skillMD},
		{"broker-poll.sh", "bash", skillBrokerPollSH},
		{"await-poll.sh", "bash", skillAwaitPollSH},
		{"broker-monitor.sh", "bash", skillBrokerMonitorSH},
	}
}

// fenceFor returns a backtick fence guaranteed longer than the longest backtick
// run inside body (minimum 3), so a body that itself contains ``` fences (the
// SKILL.md does) can't prematurely close the wrapping code block.
func fenceFor(body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// buildSkillInstallPrompt assembles the prompts/get body for skill-install by
// concatenating the embedded, debugged skill sources at request time — its
// content IS the file bytes, so there is no static markdown for this prompt.
func buildSkillInstallPrompt() string {
	var b strings.Builder

	fmt.Fprintf(&b, "**Install the broker-async-poll skill (BROKER_SKILL_VERSION=%d).**\n\n", BrokerSkillVersion)
	b.WriteString("The file bodies below are DEBUGGED, WORKING sources — the exact files the agent-broker " +
		"itself ships and embeds. They are plain `curl`+`jq` pollers of a **capability URL**: the broker's " +
		"`create_task` / `listen_role` / `get_task` results hand back a `poll_url` with an unguessable " +
		"token in the path, and the scripts just `curl \"$poll_url\"` — no auth header, nothing hidden or " +
		"fetched from elsewhere. Do **not** rewrite, \"improve\", paraphrase, reformat, or re-indent them — " +
		"save each one **verbatim**, byte for byte, exactly as given between the fences.\n\n")

	b.WriteString("Three scripts: `broker-poll.sh <poll_url>` is the WORKER waiter (blocks until a task is " +
		"queued for the role its poll_url is scoped to, picks it, exits 0); `await-poll.sh <poll_url>` is " +
		"the DISPATCHER waiter (blocks until the task you created is solved, exits 0); `broker-monitor.sh " +
		"<poll_url>` is the streaming variant (never exits — for a Monitor-style tool). Get the `poll_url` " +
		"from the tool result, arm ONE poller for it in the background, and keep working — the harness wakes " +
		"you when the script exits. The token in the poll_url expires (sliding ~30 min, hard cap 24 h); when " +
		"it does, the script exits 5 and you call listen_role / get_task again for a fresh poll_url.\n\n")

	b.WriteString("**Dependencies:** both scripts need `bash`, `curl`, and `jq` on PATH. Before installing, " +
		"verify with `command -v curl jq`; if either is missing, install it via the platform package " +
		"manager (e.g. `apt-get install -y jq` on Debian/Ubuntu, `brew install jq` on macOS — `curl` is " +
		"almost always already present). Do this before writing the scripts, so the skill is runnable " +
		"immediately after install.\n\n")

	fmt.Fprintf(&b, "**Where to put them.** The `%s/` path shown in the headers below is **Claude Code's** "+
		"skill convention. If your harness is **not** Claude Code, do NOT write into another harness's "+
		"`.claude/` namespace — it won't be auto-discovered there and just litters the project. Instead "+
		"place the three `.sh` scripts wherever your own engine keeps executable helpers (its skills / "+
		"extensions / tools directory, or any dir you can run them from): the path is not load-bearing, "+
		"only that you can invoke the scripts and read `SKILL.md` next to them for usage. Everything "+
		"below is identical either way — only the target directory changes.\n\n", skillInstallTargetDir)

	for _, f := range skillInstallFiles() {
		fmt.Fprintf(&b, "## `%s/%s`\n\n", skillInstallTargetDir, f.name)
		// Use a fence longer than any backtick run inside the body so a file that
		// itself contains ``` fences (SKILL.md does) can't close the wrapper early.
		fence := fenceFor(f.body)
		fmt.Fprintf(&b, "%s%s\n%s\n%s\n\n", fence, f.lang, strings.TrimRight(f.body, "\n"), fence)
	}

	b.WriteString("**Create or update:**\n" +
		"- If your target directory (default `" + skillInstallTargetDir + "/` for Claude Code — see " +
		"\"Where to put them\") does not exist: create it, write all three files above verbatim there, " +
		"then `chmod +x` the two `.sh` files.\n" +
		"- If it already exists: open its `broker-poll.sh` (or `await-poll.sh`) and read the " +
		"`BROKER_SKILL_VERSION=<n>` header comment near the top. Only overwrite the three files if this " +
		"prompt's version (" + fmt.Sprint(BrokerSkillVersion) + ") is **newer** than what's installed; if " +
		"it's the same or older, leave the installed copy alone. If the installed copy has **no** " +
		"`BROKER_SKILL_VERSION` marker at all (an older install predating versioning), treat it as older " +
		"and update it. When you do overwrite, replace all three files verbatim — don't hand-merge — then " +
		"re-`chmod +x` the two `.sh` files. Writing them verbatim preserves `SKILL.md`'s YAML frontmatter " +
		"(the `---`-delimited `name`/`description` block), which is what makes the skill discoverable by " +
		"intent — don't strip it.\n\n" +
		"**After install:** async broker orchestration works in this project — arm `broker-poll.sh` to " +
		"take tasks for a role, or `await-poll.sh` to be woken when a task you dispatched is solved.\n")

	return b.String()
}
