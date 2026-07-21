package common

import "testing"

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"5551234567":       "15551234567",
		"+15551234567":     "15551234567",
		"(555) 123-4567":   "15551234567",
		"1 555 123 4567":   "15551234567",
		"+44 20 7946 0958": "442079460958",
	}
	for in, want := range cases {
		if got := NormalizePhone(in); got != want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatPhone(t *testing.T) {
	if got := FormatPhone("5551234567"); got != "+1 (555) 123-4567" {
		t.Errorf("FormatPhone = %q", got)
	}
	if got := FormatPhone("442079460958"); got != "+442079460958" {
		t.Errorf("FormatPhone non-NANP = %q", got)
	}
}

func TestPortalIDRoundtrip(t *testing.T) {
	id := PortalIDFor("5551234567", "+1 (555) 987-6543")
	if string(id) != "sms:15551234567:15559876543" {
		t.Errorf("PortalIDFor = %q", id)
	}
	did, peer, ok := ParsePortalID(id)
	if !ok || did != "15551234567" || peer != "15559876543" {
		t.Errorf("ParsePortalID = %q %q %v", did, peer, ok)
	}
	if _, _, ok := ParsePortalID("thread:whatever"); ok {
		t.Error("foreign portal ID should not parse")
	}
}

func TestDIDPortalIDRoundtrip(t *testing.T) {
	id := DIDPortalIDFor("5551234567")
	if string(id) != "did:15551234567" {
		t.Errorf("DIDPortalIDFor = %q", id)
	}
	did, ok := ParseDIDPortalID(id)
	if !ok || did != "15551234567" {
		t.Errorf("ParseDIDPortalID = %q %v", did, ok)
	}
	if _, ok := ParseDIDPortalID(PortalIDFor("5551234567", "5559876543")); ok {
		t.Error("conversation portal ID should not parse as DID portal")
	}
	if _, ok := ParseDIDPortalID("did:"); ok {
		t.Error("empty DID should not parse")
	}
	// A DID space portal must never be mistaken for a conversation portal.
	if _, _, ok := ParsePortalID(id); ok {
		t.Error("DID portal ID should not parse as conversation portal")
	}
}

func TestMessageIDFor(t *testing.T) {
	if got := MessageIDFor(false, "123"); string(got) != "sms:123" {
		t.Errorf("MessageIDFor sms = %q", got)
	}
	if got := MessageIDFor(true, "123"); string(got) != "mms:123" {
		t.Errorf("MessageIDFor mms = %q", got)
	}
}
