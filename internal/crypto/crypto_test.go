package crypto

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	k := TenantKey([]byte("master"), "tenant-a")
	blob, err := Seal(k, []byte("hello backup"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Open(k, blob, []byte("aad"))
	if err != nil || !bytes.Equal(pt, []byte("hello backup")) {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestTenantIsolationAndTamper(t *testing.T) {
	ka := TenantKey([]byte("master"), "tenant-a")
	kb := TenantKey([]byte("master"), "tenant-b")
	blob, _ := Seal(ka, []byte("secret"), []byte("aad"))
	if _, err := Open(kb, blob, []byte("aad")); err == nil {
		t.Fatal("tenant B must not open tenant A data")
	}
	if _, err := Open(ka, blob, []byte("other-aad")); err == nil {
		t.Fatal("wrong AAD must fail")
	}
	blob[len(blob)-1] ^= 1
	if _, err := Open(ka, blob, []byte("aad")); err == nil {
		t.Fatal("tampered blob must fail")
	}
}
