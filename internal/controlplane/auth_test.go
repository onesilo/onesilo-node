package controlplane

import (
	"errors"
	"testing"
)

func TestJWTStoreEmptyErrors(t *testing.T) {
	s := &JWTStore{}
	_, err := s.Token()
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("expected ErrNoToken, got %v", err)
	}
}

func TestJWTStoreSwapLive(t *testing.T) {
	s := &JWTStore{}
	s.Set("jwt-one")
	if tok, err := s.Token(); err != nil || tok != "jwt-one" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	s.Set("jwt-two")
	if tok, _ := s.Token(); tok != "jwt-two" {
		t.Fatalf("swap not live: %q", tok)
	}
	s.Set("")
	if _, err := s.Token(); !errors.Is(err, ErrNoToken) {
		t.Fatal("cleared store should error")
	}
}

func TestAPIKeyStore(t *testing.T) {
	env := map[string]string{}
	s := NewAPIKeyStoreWithLookup(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if _, err := s.Token(); !errors.Is(err, ErrNoToken) {
		t.Fatal("missing SILO_API_KEY should error")
	}
	env[APIKeyEnvVar] = "sc_test_123"
	if tok, err := s.Token(); err != nil || tok != "sc_test_123" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
}

func TestModalTokenSourceSwitchesLive(t *testing.T) {
	jwt := &JWTStore{}
	jwt.Set("jwt-token")
	apiKey := NewAPIKeyStoreWithLookup(func(string) (string, bool) { return "sc_key", true })
	mode := "jwt"
	src := &ModalTokenSource{Mode: func() string { return mode }, JWT: jwt, APIKey: apiKey}

	if tok, _ := src.Token(); tok != "jwt-token" {
		t.Fatalf("jwt mode: %q", tok)
	}
	mode = "api_key"
	if tok, _ := src.Token(); tok != "sc_key" {
		t.Fatalf("api_key mode: %q", tok)
	}
}
