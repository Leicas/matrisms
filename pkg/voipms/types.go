package voipms

import "time"

// Message is a normalized VoIP.ms SMS or MMS message, as returned by the
// getSMS/getMMS API methods or reconstructed from a webhook callback.
type Message struct {
	// ID is the VoIP.ms numeric message id (string form, as the API returns
	// it). SMS and MMS live in separate id spaces — check IsMMS.
	ID string
	// IsMMS reports whether this message came from the MMS stream.
	IsMMS bool
	// Inbound is true for messages received by the DID (API type=1), false
	// for messages sent from the DID (type=0).
	Inbound bool
	// DID is our VoIP.ms number, normalized digits.
	DID string
	// Contact is the remote peer's number, normalized digits.
	Contact string
	// Body is the text content (may be empty for media-only MMS).
	Body string
	// Date is the message timestamp (converted to UTC).
	Date time.Time
	// Media holds URLs of MMS media attachments, if any.
	Media []string
}

// DID describes one phone number on the account, as returned by getDIDsInfo.
type DID struct {
	Number      string // normalized digits
	Description string
	SMSEnabled  bool
	MMSEnabled  bool
}
