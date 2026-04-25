package main

import (
	"os"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	// Create a temporary .env file
	content := "TEST_KEY=test_value\n# Comment\n  SPACED_KEY  =  spaced_value  \nQUOTED_KEY=\"quoted_value\"\n"
	err := os.WriteFile(".env", []byte(content), 0644)
	if err != nil {
		t.Fatalf("Failed to create .env: %v", err)
	}
	defer os.Remove(".env")

	loadDotEnv()

	if os.Getenv("TEST_KEY") != "test_value" {
		t.Errorf("Expected TEST_KEY=test_value, got %s", os.Getenv("TEST_KEY"))
	}
	if os.Getenv("SPACED_KEY") != "spaced_value" {
		t.Errorf("Expected SPACED_KEY=spaced_value, got %s", os.Getenv("SPACED_KEY"))
	}
	if os.Getenv("QUOTED_KEY") != "quoted_value" {
		t.Errorf("Expected QUOTED_KEY=quoted_value, got %s", os.Getenv("QUOTED_KEY"))
	}
}
