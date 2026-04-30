package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBroker_ListPrompts_UsesFrontMatterOrder(t *testing.T) {
	promptsDir := t.TempDir()

	err := os.WriteFile(filepath.Join(promptsDir, "20-second.md"), []byte("---\nname: second\ndescription: Second prompt\norder: 20\n---\nBody"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(promptsDir, "10-first.md"), []byte("---\nname: first\ndescription: First prompt\norder: 10\narguments:\n  - name: role_name\n    description: Custom role\n---\nBody"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	broker, err := NewBroker(newTestStore(t), promptsDir, true, true)
	if err != nil {
		t.Fatal(err)
	}

	prompts, err := broker.ListPrompts()
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(prompts))
	}
	if prompts[0].Name != "first" || prompts[1].Name != "second" {
		t.Fatalf("unexpected prompt order: %+v", prompts)
	}
	if len(prompts[0].Arguments) != 1 || prompts[0].Arguments[0].Name != "role_name" {
		t.Fatalf("expected role_name argument metadata, got %+v", prompts[0].Arguments)
	}
}

func TestBroker_GetPrompt_RendersRoleName(t *testing.T) {
	promptsDir := t.TempDir()

	err := os.WriteFile(filepath.Join(promptsDir, "01-worker.md"), []byte("---\nname: worker\ndescription: Worker prompt\n---\nListen on `{{role_name}}`."), 0644)
	if err != nil {
		t.Fatal(err)
	}

	broker, err := NewBroker(newTestStore(t), promptsDir, true, true)
	if err != nil {
		t.Fatal(err)
	}

	meta, content, err := broker.GetPrompt("worker", map[string]string{"role_name": "some_microservice_coder"})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Description != "Worker prompt" {
		t.Fatalf("unexpected description: %q", meta.Description)
	}
	if content != "Listen on `some_microservice_coder`." {
		t.Fatalf("unexpected content: %q", content)
	}

	_, content, err = broker.GetPrompt("worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if content != "Listen on `coder`." {
		t.Fatalf("expected default role_name substitution, got %q", content)
	}
}

func TestBroker_GetPrompt_RendersGenericArguments(t *testing.T) {
	promptsDir := t.TempDir()

	err := os.WriteFile(filepath.Join(promptsDir, "02-reviewer.md"), []byte("---\nname: reviewer\ndescription: Reviewer prompt\n---\nFocus: {{review_focus}} on {{role_name}}."), 0644)
	if err != nil {
		t.Fatal(err)
	}

	broker, err := NewBroker(newTestStore(t), promptsDir, true, true)
	if err != nil {
		t.Fatal(err)
	}

	_, content, err := broker.GetPrompt("reviewer", map[string]string{
		"role_name":    "reviewer",
		"review_focus": "correctness",
	})
	if err != nil {
		t.Fatal(err)
	}
	if content != "Focus: correctness on reviewer." {
		t.Fatalf("unexpected content: %q", content)
	}
}
