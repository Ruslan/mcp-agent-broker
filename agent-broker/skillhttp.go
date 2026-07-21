package main

import (
	"net/http"
	"strconv"
)

// skillInstallHTTP serves the skill-install instructions over plain HTTP, for
// harnesses that cannot pull MCP prompts (prompts/get). The body is byte-for-byte
// the same as the skill-install MCP prompt — the embedded, debugged poller
// scripts inline plus save-verbatim instructions — returned as text/markdown.
//
// It needs no authorization (AuthMiddleware exempts the /skill/ prefix, like
// /poll/): the content is non-secret — open-source scripts and instructions, no
// tokens or keys (poll_urls are minted per tool call, never baked in here). So a
// bare `wget http://host:PORT/skill/install` hands any agent a self-contained
// installer with no credential.
func skillInstallHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("X-Broker-Skill-Version", strconv.Itoa(BrokerSkillVersion))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(buildSkillInstallPrompt()))
}
