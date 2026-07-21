package voipms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// DefaultBaseURL is the VoIP.ms REST endpoint. Never use the www. host —
// it 301-redirects and drops POST bodies, which the API answers with
// "missing_method".
const DefaultBaseURL = "https://voip.ms/api/v1/rest.php"

// The API's date filters and returned timestamps are in the timezone
// requested via the `timezone` numeric-offset param. We always request -5
// (US Eastern, no DST adjustment) and parse timestamps with the matching
// fixed offset, so cursor math is deterministic year-round.
const apiUTCOffsetHours = -5

var apiLocation = time.FixedZone("VOIPMS", apiUTCOffsetHours*3600)

// APIError is a non-"success" status returned by the VoIP.ms API.
type APIError struct {
	Method string
	Status string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("voip.ms %s: %s", e.Method, e.Status)
}

// IsAuthError reports whether the error means the credentials (or API
// enablement / IP whitelist) are bad, i.e. retrying is pointless.
func IsAuthError(err error) bool {
	var apiErr *APIError
	if !errorsAs(err, &apiErr) {
		return false
	}
	switch apiErr.Status {
	case "invalid_credentials", "missing_credentials", "api_not_enabled", "ip_not_enabled":
		return true
	}
	return false
}

// errorsAs is a tiny local wrapper to avoid importing errors just for As.
func errorsAs(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// emptyStatuses are API statuses that mean "no data", not "error".
var emptyStatuses = map[string]bool{
	"no_sms":       true,
	"no_mms":       true,
	"no_did":       true,
	"invalid_did":  true, // per-DID SMS-not-enabled; skip, don't fatal
	"no_phonebook": true,
}

// Client is a minimal VoIP.ms REST API client covering the SMS/MMS surface.
type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client
	Log      *zerolog.Logger
}

func NewClient(baseURL, username, password string, log *zerolog.Logger) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		BaseURL:  baseURL,
		Username: username,
		Password: password,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
		Log:      log,
	}
}

// call performs one API request. All methods share the single rest.php
// endpoint with a `method` param. The body MUST be multipart/form-data:
// rest.php parses a form-urlencoded POST body as a SOAP envelope and
// faults with HTTP 500 (the official Android client also posts multipart).
// GET works too, but sendMMS media payloads don't fit in a URL.
func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	form := url.Values{}
	form.Set("api_username", c.Username)
	form.Set("api_password", c.Password)
	form.Set("method", method)
	form.Set("content_type", "json")
	for k, vs := range params {
		for _, v := range vs {
			form.Add(k, v)
		}
	}

	var reqBody bytes.Buffer
	mw := multipart.NewWriter(&reqBody)
	for k, vs := range form {
		for _, v := range vs {
			if err := mw.WriteField(k, v); err != nil {
				return fmt.Errorf("voip.ms %s: build request: %w", method, err)
			}
		}
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("voip.ms %s: build request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, &reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("voip.ms %s: %w", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024))
	if err != nil {
		return fmt.Errorf("voip.ms %s: read response: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("voip.ms %s: HTTP %d", method, resp.StatusCode)
	}

	var statusOnly struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &statusOnly); err != nil {
		return fmt.Errorf("voip.ms %s: bad JSON: %w", method, err)
	}
	if statusOnly.Status != "success" {
		if emptyStatuses[statusOnly.Status] {
			return nil // leave `out` at its zero value: empty result
		}
		return &APIError{Method: method, Status: statusOnly.Status}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("voip.ms %s: decode payload: %w", method, err)
		}
	}
	return nil
}

// flexString tolerates VoIP.ms returning a field as either a JSON string or
// a number (the API is not consistent across methods).
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" {
		*f = ""
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*f = flexString(v)
		return nil
	}
	*f = flexString(s)
	return nil
}

// VerifyCredentials checks the credentials by listing DIDs (the same probe
// the official Android client uses). Returns the SMS-capable DIDs.
func (c *Client) VerifyCredentials(ctx context.Context) ([]DID, error) {
	return c.GetSMSDIDs(ctx)
}

// GetSMSDIDs returns the account's DIDs that have SMS available, with their
// enablement state.
func (c *Client) GetSMSDIDs(ctx context.Context) ([]DID, error) {
	var out struct {
		DIDs []struct {
			DID          flexString `json:"did"`
			Description  flexString `json:"description"`
			SMSAvailable flexString `json:"sms_available"`
			SMSEnabled   flexString `json:"sms_enabled"`
			MMSAvailable flexString `json:"mms_available"`
		} `json:"dids"`
	}
	if err := c.call(ctx, "getDIDsInfo", url.Values{}, &out); err != nil {
		return nil, err
	}
	var dids []DID
	for _, d := range out.DIDs {
		if string(d.SMSAvailable) != "1" && string(d.SMSEnabled) != "1" {
			continue
		}
		dids = append(dids, DID{
			Number:      NormalizeNumber(string(d.DID)),
			Description: string(d.Description),
			SMSEnabled:  string(d.SMSEnabled) == "1",
			MMSEnabled:  string(d.MMSAvailable) == "1",
		})
	}
	return dids, nil
}

// NormalizeNumber strips non-digits from a phone number and canonicalizes
// 10-digit NANP numbers to 11 digits with the leading 1.
func NormalizeNumber(number string) string {
	var b strings.Builder
	for _, r := range number {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if len(digits) == 10 {
		digits = "1" + digits
	}
	return digits
}

// apiDID converts a normalized number back to the 10-digit form the API
// expects for US/Canada DIDs and destinations.
func apiDID(number string) string {
	n := NormalizeNumber(number)
	if len(n) == 11 && n[0] == '1' {
		return n[1:]
	}
	return n
}

// decodeBody undoes the URL-encoding VoIP.ms applies to message bodies
// (spaces come back as '+').
func decodeBody(raw string) string {
	if decoded, err := url.QueryUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

// messageRow is the wire shape of one getSMS/getMMS row. Both methods return
// the row array under the JSON key "sms".
type messageRow struct {
	ID      flexString `json:"id"`
	Date    flexString `json:"date"`
	Type    flexString `json:"type"` // "1" received, "0" sent
	DID     flexString `json:"did"`
	Contact flexString `json:"contact"`
	Message flexString `json:"message"`
	Media1  flexString `json:"col_media1"`
	Media2  flexString `json:"col_media2"`
	Media3  flexString `json:"col_media3"`
}

func (c *Client) fetchMessageRows(ctx context.Context, method, did string, from, to time.Time) ([]messageRow, error) {
	params := url.Values{}
	params.Set("did", apiDID(did))
	params.Set("limit", "1000000")
	params.Set("from", from.In(apiLocation).Format("2006-01-02"))
	params.Set("to", to.In(apiLocation).Format("2006-01-02"))
	params.Set("timezone", fmt.Sprintf("%d", apiUTCOffsetHours))

	var out struct {
		Rows []messageRow `json:"sms"`
	}
	if err := c.call(ctx, method, params, &out); err != nil {
		return nil, err
	}
	return out.Rows, nil
}

func (row *messageRow) toMessage(isMMS bool) Message {
	var media []string
	for _, m := range []string{string(row.Media1), string(row.Media2), string(row.Media3)} {
		if m != "" && m != "null" {
			media = append(media, m)
		}
	}
	date, err := time.ParseInLocation("2006-01-02 15:04:05", string(row.Date), apiLocation)
	if err != nil {
		date = time.Now()
	}
	return Message{
		ID:      string(row.ID),
		IsMMS:   isMMS,
		Inbound: string(row.Type) == "1",
		DID:     NormalizeNumber(string(row.DID)),
		Contact: NormalizeNumber(string(row.Contact)),
		Body:    decodeBody(string(row.Message)),
		Date:    date.UTC(),
		Media:   media,
	}
}

// GetMessages fetches all SMS and MMS for one DID in [from, to] (dates are
// day-granular in the API's -5 timezone; the range must stay under 92 days).
//
// SMS and MMS are fetched with separate getSMS/getMMS calls instead of the
// single getMMS+all_messages=1 merge: the merged list only reveals a row as
// MMS through its col_media* columns, which the API frequently omits — an
// image-only MMS would then be misclassified as an empty SMS and dropped.
// With a definitive per-method type, media-less MMS rows survive and their
// attachments are recovered later via getMediaMMS.
func (c *Client) GetMessages(ctx context.Context, did string, from, to time.Time) ([]Message, error) {
	mmsRows, err := c.fetchMessageRows(ctx, "getMMS", did, from, to)
	if err != nil {
		return nil, err
	}
	smsRows, err := c.fetchMessageRows(ctx, "getSMS", did, from, to)
	if err != nil {
		return nil, err
	}

	msgs := make([]Message, 0, len(mmsRows)+len(smsRows))
	// Guard against getSMS mirroring MMS rows (observed API behavior varies):
	// drop any SMS row that exactly matches an MMS row from the same batch.
	seenMMS := make(map[string]bool, len(mmsRows))
	for i := range mmsRows {
		msg := mmsRows[i].toMessage(true)
		msgs = append(msgs, msg)
		seenMMS[msg.Date.Format(time.RFC3339)+"|"+msg.Contact+"|"+fmt.Sprintf("%t", msg.Inbound)+"|"+msg.Body] = true
	}
	for i := range smsRows {
		msg := smsRows[i].toMessage(false)
		if msg.Body == "" {
			continue // malformed row, mirror the Android client's skip
		}
		if seenMMS[msg.Date.Format(time.RFC3339)+"|"+msg.Contact+"|"+fmt.Sprintf("%t", msg.Inbound)+"|"+msg.Body] {
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, nil
}

// PhonebookEntry is one VoIP.ms phonebook contact.
type PhonebookEntry struct {
	// ID is the numeric phonebook entry id (string form).
	ID string
	// Name is the contact's display name.
	Name string
	// Number is the contact's phone number, normalized digits. Internal
	// speed-dial entries (SIP accounts etc.) have short or empty numbers.
	Number string
	// SpeedDial, CallerID and Note are carried through so updates via
	// setPhonebook don't wipe them.
	SpeedDial string
	CallerID  string
	Note      string
}

// GetPhonebook fetches all phonebook entries on the account.
func (c *Client) GetPhonebook(ctx context.Context) ([]PhonebookEntry, error) {
	var out struct {
		Entries []struct {
			ID        flexString `json:"phonebook"`
			SpeedDial flexString `json:"speed_dial"`
			Name      flexString `json:"name"`
			Number    flexString `json:"number"`
			CallerID  flexString `json:"callerid"`
			Note      flexString `json:"note"`
		} `json:"phonebooks"`
	}
	if err := c.call(ctx, "getPhonebook", url.Values{}, &out); err != nil {
		return nil, err
	}
	entries := make([]PhonebookEntry, 0, len(out.Entries))
	for _, e := range out.Entries {
		entries = append(entries, PhonebookEntry{
			ID:        string(e.ID),
			Name:      string(e.Name),
			Number:    NormalizeNumber(string(e.Number)),
			SpeedDial: string(e.SpeedDial),
			CallerID:  string(e.CallerID),
			Note:      string(e.Note),
		})
	}
	return entries, nil
}

// SetContactName creates or renames the phonebook entry for a number and
// returns the resulting entry. Existing entries keep their speed dial,
// caller ID and note.
func (c *Client) SetContactName(ctx context.Context, number, name string) (*PhonebookEntry, error) {
	number = NormalizeNumber(number)
	entries, err := c.GetPhonebook(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Number != number {
			continue
		}
		params := url.Values{}
		params.Set("phonebook", e.ID)
		params.Set("name", name)
		params.Set("number", apiDID(e.Number))
		if e.SpeedDial != "" {
			params.Set("speed_dial", e.SpeedDial)
		}
		if e.CallerID != "" {
			params.Set("callerid", e.CallerID)
		}
		if e.Note != "" {
			params.Set("note", e.Note)
		}
		if err := c.call(ctx, "setPhonebook", params, nil); err != nil {
			return nil, err
		}
		e.Name = name
		return &e, nil
	}

	params := url.Values{}
	params.Set("name", name)
	params.Set("number", apiDID(number))
	var out struct {
		Phonebook json.Number `json:"phonebook"`
	}
	if err := c.call(ctx, "addPhonebook", params, &out); err != nil {
		return nil, err
	}
	return &PhonebookEntry{ID: out.Phonebook.String(), Name: name, Number: number}, nil
}

// GetMMSMedia fetches the media URL list for one MMS by id. getMMS
// frequently returns empty col_media fields even for image MMS; this is the
// reliable way to discover attachments.
func (c *Client) GetMMSMedia(ctx context.Context, id string) ([]string, error) {
	params := url.Values{}
	params.Set("id", id)
	params.Set("media_as_array", "1")
	var out struct {
		Media []flexString `json:"media"`
	}
	if err := c.call(ctx, "getMediaMMS", params, &out); err != nil {
		return nil, err
	}
	var media []string
	for _, m := range out.Media {
		if m != "" {
			media = append(media, string(m))
		}
	}
	return media, nil
}

// SMSMaxLen is the per-message limit enforced by sendSMS (sms_toolong past this).
const SMSMaxLen = 160

// MMSMaxTextLen is the text limit for sendMMS.
const MMSMaxTextLen = 2048

// MMSMaxMediaBytes is the per-attachment media cap (~1.3 MB per the wiki;
// keep a little headroom for base64 overhead).
const MMSMaxMediaBytes = 1300 * 1024

// SplitSMS chunks a long text into ≤160-rune segments on rune boundaries,
// preferring to break at the last space in each window.
func SplitSMS(message string) []string {
	runes := []rune(message)
	if len(runes) <= SMSMaxLen {
		return []string{message}
	}
	var parts []string
	for len(runes) > 0 {
		if len(runes) <= SMSMaxLen {
			parts = append(parts, string(runes))
			break
		}
		cut := SMSMaxLen
		for i := SMSMaxLen - 1; i > SMSMaxLen-40 && i > 0; i-- {
			if runes[i] == ' ' {
				cut = i
				break
			}
		}
		parts = append(parts, strings.TrimRight(string(runes[:cut]), " "))
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == ' ' {
			runes = runes[1:]
		}
	}
	return parts
}

// SendSMS sends one text message (≤160 chars) and returns the new VoIP.ms
// message id. Longer texts must be split by the caller (see SplitSMS).
func (c *Client) SendSMS(ctx context.Context, did, dst, message string) (string, error) {
	params := url.Values{}
	params.Set("did", apiDID(did))
	params.Set("dst", apiDID(dst))
	params.Set("message", message)
	var out struct {
		SMS json.Number `json:"sms"`
	}
	if err := c.call(ctx, "sendSMS", params, &out); err != nil {
		return "", err
	}
	if out.SMS.String() == "" || out.SMS.String() == "0" {
		return "", fmt.Errorf("voip.ms sendSMS: no message id in response")
	}
	return out.SMS.String(), nil
}

// MediaUpload is one MMS attachment to send.
type MediaUpload struct {
	Data []byte
	Mime string
}

// SendMMS sends a message with up to 3 media attachments (each ≤~1.3 MB;
// JPG/GIF/PNG/MP3/WAV/MIDI/MP4/3GP) and returns the new VoIP.ms message id.
// Media is passed as data: URIs; text may be up to 2048 chars.
func (c *Client) SendMMS(ctx context.Context, did, dst, message string, media []MediaUpload) (string, error) {
	if len(media) > 3 {
		return "", fmt.Errorf("voip.ms sendMMS: at most 3 media attachments (got %d)", len(media))
	}
	params := url.Values{}
	params.Set("did", apiDID(did))
	params.Set("dst", apiDID(dst))
	params.Set("message", message)
	for i, m := range media {
		if len(m.Data) > MMSMaxMediaBytes {
			return "", fmt.Errorf("voip.ms sendMMS: attachment %d exceeds %d bytes", i+1, MMSMaxMediaBytes)
		}
		dataURI := fmt.Sprintf("data:%s;base64,%s", m.Mime, base64.StdEncoding.EncodeToString(m.Data))
		params.Set(fmt.Sprintf("media%d", i+1), dataURI)
	}
	var out struct {
		MMS json.Number `json:"mms"`
	}
	if err := c.call(ctx, "sendMMS", params, &out); err != nil {
		return "", err
	}
	if out.MMS.String() == "" || out.MMS.String() == "0" {
		return "", fmt.Errorf("voip.ms sendMMS: no message id in response")
	}
	return out.MMS.String(), nil
}

// DownloadMedia fetches an MMS media URL (plain HTTPS, no auth) and returns
// the bytes plus the reported content type.
func (c *Client) DownloadMedia(ctx context.Context, mediaURL string, maxBytes int) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("media download: HTTP %d", resp.StatusCode)
	}
	limit := int64(maxBytes)
	if limit <= 0 {
		limit = 64 * 1024 * 1024
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("media download: exceeds %d byte limit", limit)
	}
	mime := resp.Header.Get("Content-Type")
	if idx := strings.Index(mime, ";"); idx >= 0 {
		mime = mime[:idx]
	}
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}
