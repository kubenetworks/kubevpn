package ssh

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/types"
)

// These tests lock the wire-level behavior of the GSSAPI/Kerberos code BEFORE
// any restructuring, so a byte-format or state-machine regression is caught even
// without Kerberos infrastructure. The logic under test must stay behaviorally
// identical across the pkg/ssh rewrite.

// TestCCache_MarshalUnmarshal_RoundTrip locks the V4 ccache binary encoding: a
// CCache marshaled, parsed back, and re-marshaled must produce identical bytes.
func TestCCache_MarshalUnmarshal_RoundTrip(t *testing.T) {
	c := NewV4CCache()
	c.SetDefaultPrincipal(NewPrincipal(
		types.PrincipalName{NameType: 1, NameString: []string{"alice"}},
		"EXAMPLE.COM",
	))
	c.AddCredential(&Credential{
		Client: NewPrincipal(
			types.PrincipalName{NameType: 1, NameString: []string{"alice"}},
			"EXAMPLE.COM",
		),
		Server: NewPrincipal(
			types.PrincipalName{NameType: 2, NameString: []string{"krbtgt", "EXAMPLE.COM"}},
			"EXAMPLE.COM",
		),
		Key:         types.EncryptionKey{KeyType: 18, KeyValue: []byte("0123456789abcdef0123456789abcdef")},
		TicketFlags: types.NewKrbFlags(),
		Ticket:      []byte("ticket-bytes"),
	})

	b1, err := c.Marshal()
	if err != nil {
		t.Fatalf("first Marshal: %v", err)
	}

	var c2 CCache
	if err = c2.Unmarshal(b1); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	b2, err := c2.Marshal()
	if err != nil {
		t.Fatalf("second Marshal: %v", err)
	}

	if !bytes.Equal(b1, b2) {
		t.Fatalf("round-trip not byte-identical:\n first=%x\nsecond=%x", b1, b2)
	}

	// Spot-check parsed fields survived the round-trip.
	if got := c2.GetClientRealm(); got != "EXAMPLE.COM" {
		t.Errorf("client realm = %q, want EXAMPLE.COM", got)
	}
	if len(c2.Credentials) != 1 {
		t.Fatalf("credentials = %d, want 1", len(c2.Credentials))
	}
	if got := string(c2.Credentials[0].Ticket); got != "ticket-bytes" {
		t.Errorf("ticket = %q, want ticket-bytes", got)
	}
}

// TestCCache_Unmarshal_RejectsBadMagic locks the leading-byte validation.
func TestCCache_Unmarshal_RejectsBadMagic(t *testing.T) {
	var c CCache
	if err := c.Unmarshal([]byte{0x04, 0x04}); err == nil {
		t.Fatal("expected error for first byte != 5, got nil")
	}
}

// TestNewAuthenticatorChksum locks the hand-built 24/28-byte GSSAPI checksum
// field layout (little-endian length prefix + OR-ed flags at [20:24]).
func TestNewAuthenticatorChksum(t *testing.T) {
	k := &Krb5InitiatorClient{}

	flags := []int{ContextFlagREADY, gssapi.ContextFlagInteg, gssapi.ContextFlagMutual}
	chksum := k.newAuthenticatorChksum(flags)
	if len(chksum) != 24 {
		t.Fatalf("checksum length = %d, want 24", len(chksum))
	}
	if got := binary.LittleEndian.Uint32(chksum[:4]); got != 16 {
		t.Errorf("length prefix = %d, want 16", got)
	}
	wantFlags := uint32(ContextFlagREADY | gssapi.ContextFlagInteg | gssapi.ContextFlagMutual)
	if got := binary.LittleEndian.Uint32(chksum[20:24]); got != wantFlags {
		t.Errorf("flags word = %#x, want %#x", got, wantFlags)
	}

	// Delegation extends the field to 28 bytes.
	withDeleg := k.newAuthenticatorChksum(append(append([]int{}, flags...), gssapi.ContextFlagDeleg))
	if len(withDeleg) != 28 {
		t.Fatalf("checksum length with delegation = %d, want 28", len(withDeleg))
	}
}

// TestKrb5InitiatorClient_ReadyStateRejectsReuse locks the terminal state-machine
// guard: calling InitSecContext after reaching Ready is an error.
func TestKrb5InitiatorClient_ReadyStateRejectsReuse(t *testing.T) {
	k := &Krb5InitiatorClient{state: InitiatorReady}
	if _, _, err := k.InitSecContext("host@REALM", nil, false); err == nil {
		t.Fatal("expected error when reusing a Ready initiator, got nil")
	}

	k = &Krb5InitiatorClient{state: Krb5ClientState(99)}
	if _, _, err := k.InitSecContext("host@REALM", nil, false); err == nil {
		t.Fatal("expected error for invalid state, got nil")
	}
}
