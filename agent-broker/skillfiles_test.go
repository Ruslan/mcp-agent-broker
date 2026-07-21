package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestSkillScriptsParse runs `bash -n` on the canonical poller scripts so a
// syntax slip (e.g. a botched parameter expansion) fails the Go test suite rather
// than only at runtime on a live poll. Skips when bash isn't on PATH (some CI
// images lack it) — the scripts are bash-only anyway.
func TestSkillScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH; skipping shell syntax check")
	}
	for _, name := range []string{"broker-poll.sh", "await-poll.sh", "broker-monitor.sh"} {
		name := name
		t.Run(name, func(t *testing.T) {
			path := "skillfiles/" + name
			out, err := exec.Command(bash, "-n", path).CombinedOutput()
			if err != nil {
				t.Fatalf("bash -n %s failed: %v\n%s", path, err, out)
			}
		})
	}
}

// installSkillDir is the GENERATED install copy of the broker-async-poll skill
// (produced from the canonical agent-broker/skillfiles/ by `make
// sync-skillfiles`). Tests run with the package dir as the working directory, so
// "../" resolves to the repo root — only `//go:embed` forbids climbing above the
// package dir; a plain os.ReadFile at test time does not.
const installSkillDir = "../.claude/skills/broker-async-poll"

// embeddedSkillfiles pairs each embedded (canonical) variable with the filename
// both agent-broker/skillfiles/<name> (canon, embedded) and
// .claude/skills/broker-async-poll/<name> (generated install copy) must share.
func embeddedSkillfiles() map[string]string {
	return map[string]string{
		"broker-poll.sh":    skillBrokerPollSH,
		"await-poll.sh":     skillAwaitPollSH,
		"broker-monitor.sh": skillBrokerMonitorSH,
		"SKILL.md":          skillMD,
	}
}

// TestSkillfilesMatchCanonical asserts the generated install copy is
// byte-identical to the canonical sources embedded from agent-broker/skillfiles/.
// This is the drift guard: if a dev edits the install copy directly, or edits the
// canon and forgets to resync, the build goes red instead of shipping a stale
// local skill or an inconsistent skill-install prompt.
func TestSkillfilesMatchCanonical(t *testing.T) {
	for name, canonical := range embeddedSkillfiles() {
		t.Run(name, func(t *testing.T) {
			installPath := installSkillDir + "/" + name
			installed, err := os.ReadFile(installPath)
			if err != nil {
				t.Fatalf("read install copy %s: %v", installPath, err)
			}
			if string(installed) != canonical {
				t.Fatalf(
					"%s has drifted from the canonical source agent-broker/skillfiles/%s.\n"+
						"Fix: run `make sync-skillfiles` to regenerate the install copy from the canon.",
					installPath, name)
			}
		})
	}
}

// brokerSkillVersionMarker matches the "# BROKER_SKILL_VERSION=<n>" header
// comment stamped near the top of broker-poll.sh and await-poll.sh.
var brokerSkillVersionMarker = regexp.MustCompile(`(?m)^#\s*BROKER_SKILL_VERSION=(\d+)\s*$`)

func extractSkillVersionMarker(t *testing.T, filename, body string) int {
	t.Helper()
	m := brokerSkillVersionMarker.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no BROKER_SKILL_VERSION=<n> marker found in embedded %s", filename)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("BROKER_SKILL_VERSION marker in %s is not an int: %q", filename, m[1])
	}
	return n
}

// TestBrokerSkillVersionMarkersMatchConstant ties the BrokerSkillVersion
// constant to the single "# BROKER_SKILL_VERSION=<n>" marker stamped in both
// scripts, so there is exactly one place a dev bumps the version and one test
// that catches it being forgotten in either script (or the constant).
func TestBrokerSkillVersionMarkersMatchConstant(t *testing.T) {
	for _, name := range []string{"broker-poll.sh", "await-poll.sh", "broker-monitor.sh"} {
		body := embeddedSkillfiles()[name]
		got := extractSkillVersionMarker(t, name, body)
		if got != BrokerSkillVersion {
			t.Fatalf("%s marker BROKER_SKILL_VERSION=%d != BrokerSkillVersion constant %d", name, got, BrokerSkillVersion)
		}
	}
}

// TestFenceForOutgrowsInnerFences pins the fix for the nested-fence bug: a file
// body that itself contains ``` fences (SKILL.md does) must be wrapped in a
// longer fence so the inner ones can't close the wrapper early.
func TestFenceForOutgrowsInnerFences(t *testing.T) {
	if got := fenceFor("no backticks here"); got != "```" {
		t.Fatalf("fenceFor(plain) = %q, want three backticks", got)
	}
	if got := fenceFor("a\n```\nb"); len(got) <= 3 {
		t.Fatalf("fenceFor(body with ```) = %q, want > 3 backticks", got)
	}
	if got := fenceFor(skillMD); len(got) <= 3 {
		t.Fatalf("fenceFor(SKILL.md) = %q, want > 3 backticks so inner fences can't close it", got)
	}
}

// TestPromptsGetSkillInstall exercises prompts/get for skill-install end to end
// via the JSON-RPC surface and pins the contract skill-install promises: each
// target path is named, the version is stated, and a recognizable snippet of
// each embedded script's body is present verbatim.
func TestPromptsGetSkillInstall(t *testing.T) {
	broker := newTestBroker(t, true, true)
	handler := &JSONRPCHandler{broker: broker}

	req := Request{
		JSONRPC: "2.0",
		Method:  "prompts/get",
		Params:  json.RawMessage(`{"name":"skill-install"}`),
		ID:      json.RawMessage(`"1"`),
	}
	resp := callHandler(handler, req, "default")
	if resp.Error != nil {
		t.Fatalf("prompts/get skill-install returned error: %+v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	messages, ok := result["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("expected exactly one message, got %v", result["messages"])
	}
	msg := messages[0].(map[string]any)
	content := msg["content"].(map[string]any)
	text, _ := content["text"].(string)

	for _, name := range []string{"broker-poll.sh", "await-poll.sh", "broker-monitor.sh", "SKILL.md"} {
		wantPath := skillInstallTargetDir + "/" + name
		if !strings.Contains(text, wantPath) {
			t.Fatalf("skill-install prompt missing target path %q", wantPath)
		}
	}

	wantVersion := "BROKER_SKILL_VERSION=" + strconv.Itoa(BrokerSkillVersion)
	if !strings.Contains(text, wantVersion) {
		t.Fatalf("skill-install prompt missing version marker %q", wantVersion)
	}

	// A recognizable, stable snippet from each embedded script's body — proof the
	// actual file bytes (not a paraphrase) made it into the prompt.
	snippets := map[string]string{
		"broker-poll.sh":    `url="${1:-${BROKER_POLL_URL:-}}"`,
		"await-poll.sh":     `curl -sL -w '\n%{http_code}' "$url"`,
		"broker-monitor.sh": "broker-monitor.sh",
		"SKILL.md":          "name: broker-async-poll",
	}
	for name, snippet := range snippets {
		if !strings.Contains(text, snippet) {
			t.Fatalf("skill-install prompt missing %s snippet %q", name, snippet)
		}
	}

	if !strings.Contains(text, "**Dependencies:**") {
		t.Fatalf("skill-install prompt missing a Dependencies section")
	}
	if !strings.Contains(text, "command -v curl jq") {
		t.Fatalf("skill-install prompt missing the curl+jq verify command")
	}
}

// TestListPromptsIncludesSkillInstall asserts skill-install shows up in
// prompts/list alongside the on-disk prompt templates.
func TestListPromptsIncludesSkillInstall(t *testing.T) {
	broker := newTestBroker(t, true, true)
	prompts, err := broker.ListPrompts()
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	found := false
	for _, p := range prompts {
		if p.Name == skillInstallPromptName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("prompts/list does not include %q", skillInstallPromptName)
	}
}
