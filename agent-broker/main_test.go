package main

import (
	"os"
	"testing"
)

func TestGetEnvBool(t *testing.T) {
	os.Setenv("TEST_ENV_BOOL_TRUE", "true")
	os.Setenv("TEST_ENV_BOOL_FALSE", "false")
	defer os.Unsetenv("TEST_ENV_BOOL_TRUE")
	defer os.Unsetenv("TEST_ENV_BOOL_FALSE")

	if !getEnvBool("TEST_ENV_BOOL_TRUE", false) {
		t.Errorf("Expected true, got false")
	}

	if getEnvBool("TEST_ENV_BOOL_FALSE", true) {
		t.Errorf("Expected false, got true")
	}

	if !getEnvBool("TEST_ENV_BOOL_UNSET", true) {
		t.Errorf("Expected default true, got false")
	}

	if getEnvBool("TEST_ENV_BOOL_UNSET", false) {
		t.Errorf("Expected default false, got true")
	}
}

func TestValidateConfig(t *testing.T) {
	if err := validateConfig(true, true); err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if err := validateConfig(true, false); err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if err := validateConfig(false, true); err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if err := validateConfig(false, false); err == nil {
		t.Errorf("Expected error, got nil")
	}
}
