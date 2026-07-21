package common

import (
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

// NormalizePhone strips everything except digits and a leading '+' from a
// phone number. VoIP.ms reports 10-digit NANP numbers without a country code;
// we canonicalize those to 11-digit 1XXXXXXXXXX so ghost IDs stay stable no
// matter which form the API returns.
func NormalizePhone(number string) string {
	number = strings.TrimSpace(number)
	var b strings.Builder
	for i, r := range number {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r == '+' && i == 0 {
			continue // dropped; we store digits only
		}
	}
	digits := b.String()
	if len(digits) == 10 {
		digits = "1" + digits
	}
	return digits
}

// FormatPhone renders a normalized number for humans: NANP numbers become
// "+1 (555) 123-4567", anything else gets a bare "+" prefix.
func FormatPhone(number string) string {
	digits := NormalizePhone(number)
	if len(digits) == 11 && digits[0] == '1' {
		return fmt.Sprintf("+1 (%s) %s-%s", digits[1:4], digits[4:7], digits[7:])
	}
	if digits == "" {
		return "unknown"
	}
	return "+" + digits
}

// PhoneToGhostID converts a phone number to a Matrix ghost user ID in a
// single, canonical place. Scheme: "sms:" + <normalized-digits>
func PhoneToGhostID(number string) networkid.UserID {
	return networkid.UserID("sms:" + NormalizePhone(number))
}

// PortalIDFor builds the portal ID for a conversation between one of our DIDs
// and a remote peer. Scheme: "sms:<did>:<peer>", both normalized. One portal
// per (DID, peer) pair so the same contact texting two of your numbers gets
// two rooms, mirroring how phones treat separate lines.
func PortalIDFor(did, peer string) networkid.PortalID {
	return networkid.PortalID(fmt.Sprintf("sms:%s:%s", NormalizePhone(did), NormalizePhone(peer)))
}

// ParsePortalID splits a portal ID back into (did, peer). Returns ok=false
// for portal IDs not created by PortalIDFor.
func ParsePortalID(id networkid.PortalID) (did, peer string, ok bool) {
	parts := strings.Split(string(id), ":")
	if len(parts) != 3 || parts[0] != "sms" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// DIDPortalIDFor builds the portal ID of the per-DID space that groups every
// conversation room of one of our numbers. Scheme: "did:<did>". These portals
// are created as Matrix spaces (RoomTypeSpace) and are set as the ParentID of
// each "sms:<did>:<peer>" conversation portal.
func DIDPortalIDFor(did string) networkid.PortalID {
	return networkid.PortalID("did:" + NormalizePhone(did))
}

// ParseDIDPortalID extracts the DID from a "did:<did>" space portal ID.
// Returns ok=false for portal IDs not created by DIDPortalIDFor.
func ParseDIDPortalID(id networkid.PortalID) (did string, ok bool) {
	raw, found := strings.CutPrefix(string(id), "did:")
	if !found || raw == "" || strings.Contains(raw, ":") {
		return "", false
	}
	return raw, true
}

// MessageIDFor builds the network message ID for a VoIP.ms message.
// Scheme: "sms:<voipms-id>" or "mms:<voipms-id>" — VoIP.ms uses separate id
// spaces for SMS and MMS, so the prefix keeps them collision-free.
func MessageIDFor(isMMS bool, voipmsID string) networkid.MessageID {
	if isMMS {
		return networkid.MessageID("mms:" + voipmsID)
	}
	return networkid.MessageID("sms:" + voipmsID)
}

// UserLoginIDFor builds the bridgev2 user-login ID for a VoIP.ms account.
// Scheme: "voipms:" + <api username (account email)>
func UserLoginIDFor(apiUsername string) networkid.UserLoginID {
	return networkid.UserLoginID("voipms:" + strings.ToLower(strings.TrimSpace(apiUsername)))
}

// RecoverToError is a common panic recovery utility that converts panics to errors.
// Use in defer statements to catch panics and return them as errors through a channel.
// Safe against nil channels, closed channels, and preserves error wrapping.
func RecoverToError(errCh chan<- error) {
	if r := recover(); r != nil {
		var err error

		// Preserve original error if the panic value is already an error
		if panicErr, ok := r.(error); ok {
			err = fmt.Errorf("panic recovered: %w", panicErr)
		} else {
			err = fmt.Errorf("panic recovered: %v", r)
		}

		// Safe send - non-blocking to prevent deadlocks and handle closed/nil channels
		if errCh != nil {
			select {
			case errCh <- err:
				// Successfully sent
			default:
				// Channel full, closed, or nil - don't block
			}
		}
	}
}
