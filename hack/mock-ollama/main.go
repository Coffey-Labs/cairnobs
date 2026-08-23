// Command mock-ollama stands in for a real Ollama server (see
// /docs/phase-7-ai-design.md) when manually verifying Phase 7's AI
// features against a live docker-compose stack without needing model
// weights or a GPU. Matches Ollama's real POST /api/chat wire contract
// (see api/ai/provider/ollama/ollama.go's chatRequest/chatResponse)
// closely enough that api/cmd/api and enterprise/cmd/enterprise-api
// can't tell the difference -- picks a canned, deterministic response by
// inspecting the system prompt's distinctive opening line (see
// api/ai/provider/ollama/prompts.go), the same technique the
// integration tests in api/ai/aiapi/integration_test.go use for the
// same reason: no live model, deterministic output, fast.
//
// Not part of any docker-compose service by default -- run it
// standalone (or as a throwaway container on the cairnobs_default
// network) and point OLLAMA_BASE_URL at it. See
// /docs/phase-7-runbook.md for the exact recipe.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatResponse struct {
	Message chatMessage `json:"message"`
}

// canned responses, keyed by a distinctive substring of each
// operation's system prompt (see prompts.go's opening sentence for
// each). Content is deliberately valid, uninteresting pipe syntax --
// this tool exists to verify plumbing, not to simulate model quality.
var canned = []struct {
	systemPromptContains string
	response             string
}{
	{"translate a plain-English question", `{"query":"earliest=-1h severity=ERROR","confidence":"high"}`},
	{"suggest how to continue", `{"suggestion":" severity=ERROR"}`},
	{"phrase those findings", "Add a time range (e.g. earliest=-1h) to avoid scanning the entire table."},
	{"fix a broken", `{"suggested_query":"earliest=-1h severity=ERROR","explanation":"added a missing time bound","confidence":"high"}`},
	// Plain explain (no findings, no original-intent framing) falls
	// through to this last, broadest match.
	{"", "This query filters logs where severity equals ERROR from the last hour."},
}

func respond(system string) string {
	for _, c := range canned {
		if strings.Contains(system, c.systemPromptContains) {
			return c.response
		}
	}
	return canned[len(canned)-1].response
}

func main() {
	addr := flag.String("addr", ":11434", "listen address")
	flag.Parse()

	http.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req chatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "decoding request: "+err.Error(), http.StatusBadRequest)
			return
		}
		var system string
		for _, m := range req.Messages {
			if m.Role == "system" {
				system = m.Content
				break
			}
		}
		content := respond(system)
		fmt.Printf("chat: model=%s -> %s\n", req.Model, content)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{Message: chatMessage{Role: "assistant", Content: content}})
	})

	log.Printf("mock-ollama listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
