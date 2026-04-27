package main

import (
	"testing"
)

func TestRPCError_Error(t *testing.T) {
	err := &RPCError{
		Code:    123,
		Message: "test error message",
	}

	if err.Error() != "test error message" {
		t.Errorf("Expected 'test error message', got '%s'", err.Error())
	}
}
