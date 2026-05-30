package mcpserver

import "testing"

func TestCancellationRegistryCancelInvokes(t *testing.T) {
	r := NewCancellationRegistry()
	cancelled := false
	r.Register(1, func() { cancelled = true })
	if r.Len() != 1 {
		t.Fatalf("len=%d want 1", r.Len())
	}
	if !r.Cancel(1) {
		t.Fatal("Cancel returned false for a registered id")
	}
	if !cancelled {
		t.Fatal("cancel func was not invoked")
	}
	if r.Len() != 0 {
		t.Fatalf("len=%d want 0 after cancel", r.Len())
	}
}

func TestCancellationRegistryDoneSuppresses(t *testing.T) {
	r := NewCancellationRegistry()
	cancelled := false
	r.Register("abc", func() { cancelled = true })
	r.Done("abc")
	if r.Cancel("abc") {
		t.Fatal("Cancel should return false after Done")
	}
	if cancelled {
		t.Fatal("cancel func must not run after Done")
	}
}

func TestCancellationRegistryStringVsNumberDistinct(t *testing.T) {
	r := NewCancellationRegistry()
	var got string
	r.Register("1", func() { got = "string" })
	r.Register(float64(1), func() { got = "number" }) // JSON numbers decode to float64
	r.Cancel("1")
	if got != "string" {
		t.Fatalf("got=%q want \"string\" (string id 1 must be distinct from number 1)", got)
	}
	if r.Len() != 1 {
		t.Fatalf("len=%d want 1 (numeric id still pending)", r.Len())
	}
}

func TestCancellationRegistryReRegisterCancelsPrev(t *testing.T) {
	r := NewCancellationRegistry()
	prevCancelled := false
	r.Register(5, func() { prevCancelled = true })
	r.Register(5, func() {}) // duplicate id -> previous cancel invoked
	if !prevCancelled {
		t.Fatal("re-registering the same id should invoke the previous cancel")
	}
}
