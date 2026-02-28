package server

import (
	"encoding/base64"
	"testing"
)

func TestNormalizeSCBConfirmPhrase(t *testing.T) {
	got := normalizeSCBConfirmPhrase("  i   understand  force   close ")
	if got != scbRecoveryConfirmPhrase {
		t.Fatalf("expected %q, got %q", scbRecoveryConfirmPhrase, got)
	}
}

func TestDecodeSCBBackup(t *testing.T) {
	payload := []byte("sample-scb-backup")
	b64 := base64.StdEncoding.EncodeToString(payload)

	got, err := decodeSCBBackup(b64)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("unexpected payload: %q", string(got))
	}

	dataURI := "data:application/octet-stream;base64," + b64
	gotURI, err := decodeSCBBackup(dataURI)
	if err != nil {
		t.Fatalf("decode data URI failed: %v", err)
	}
	if string(gotURI) != string(payload) {
		t.Fatalf("unexpected data URI payload: %q", string(gotURI))
	}
}

func TestDecodeSCBBackupInvalid(t *testing.T) {
	_, err := decodeSCBBackup("%%%")
	if err == nil {
		t.Fatalf("expected decode error")
	}
}
