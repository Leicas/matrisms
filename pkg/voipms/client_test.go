package voipms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestNormalizeNumber(t *testing.T) {
	cases := map[string]string{
		"5551234567":        "15551234567",
		"15551234567":       "15551234567",
		"+1 (555) 123-4567": "15551234567",
		"1-555-123-4567":    "15551234567",
	}
	for in, want := range cases {
		if got := NormalizeNumber(in); got != want {
			t.Errorf("NormalizeNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAPIDID(t *testing.T) {
	if got := apiDID("15551234567"); got != "5551234567" {
		t.Errorf("apiDID = %q, want 5551234567", got)
	}
}

func TestSplitSMS(t *testing.T) {
	if parts := SplitSMS("short"); len(parts) != 1 || parts[0] != "short" {
		t.Errorf("short message should be one part, got %v", parts)
	}
	long := strings.Repeat("word ", 100) // 500 chars
	parts := SplitSMS(long)
	if len(parts) < 3 {
		t.Errorf("expected >=3 parts for 500 chars, got %d", len(parts))
	}
	for i, p := range parts {
		if len([]rune(p)) > SMSMaxLen {
			t.Errorf("part %d exceeds %d chars: %d", i, SMSMaxLen, len([]rune(p)))
		}
	}
	if joined := strings.Join(parts, " "); strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(long), " ") {
		t.Error("content lost during split")
	}
}

func TestDecodeBody(t *testing.T) {
	if got := decodeBody("hello+world%21"); got != "hello world!" {
		t.Errorf("decodeBody = %q", got)
	}
}

func TestCursorRoundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c := Cursor{LastDate: now}
	decoded := DecodeCursor(c.Encode())
	if !decoded.LastDate.Equal(now) {
		t.Errorf("cursor roundtrip: got %v want %v", decoded.LastDate, now)
	}
	if !DecodeCursor("").LastDate.IsZero() {
		t.Error("empty cursor should decode to zero time")
	}
}

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	log := zerolog.Nop()
	c := NewClient(srv.URL, "user@example.com", "apipass", &log)
	return c, srv
}

func TestGetMessagesParsing(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		switch r.FormValue("method") {
		case "getSMS":
			w.Write([]byte(`{"status":"success","sms":[
				{"id":"100","date":"2026-07-20 10:00:00","type":"1","did":"5551234567","contact":"5559876543","message":"hello+there"},
				{"id":"101","date":"2026-07-20 10:05:00","type":"0","did":"5551234567","contact":"5559876543","message":"reply"},
				{"id":"103","date":"2026-07-20 10:15:00","type":"1","did":"5551234567","contact":"5559876543","message":""}
			]}`))
		case "getMMS":
			if r.FormValue("all_messages") != "" {
				t.Error("getMMS must not pass all_messages (SMS come from getSMS)")
			}
			w.Write([]byte(`{"status":"success","sms":[
				{"id":"102","date":"2026-07-20 10:10:00","type":"1","did":"5551234567","contact":"5559876543","message":"","col_media1":"https://voip.ms/media.php?x=1"},
				{"id":"104","date":"2026-07-20 10:20:00","type":"1","did":"5551234567","contact":"5559876543","message":"","col_media1":null}
			]}`))
		default:
			t.Errorf("unexpected method %s", r.FormValue("method"))
		}
	})
	msgs, err := c.GetMessages(context.Background(), "15551234567", time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// MMS rows come first (104 kept despite empty body+media: media is
	// recovered later via getMediaMMS), then SMS rows (empty 103 skipped).
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %+v", len(msgs), msgs)
	}
	byID := map[string]Message{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	if _, ok := byID["103"]; ok {
		t.Error("empty SMS row should be skipped")
	}
	if m, ok := byID["104"]; !ok || !m.IsMMS {
		t.Error("media-less MMS row should be kept and typed as MMS")
	}
	if m := byID["100"]; m.Body != "hello there" {
		t.Errorf("body not URL-decoded: %q", m.Body)
	}
	if !byID["100"].Inbound || byID["101"].Inbound {
		t.Error("type field parsed wrong")
	}
	if m := byID["100"]; m.Contact != "15559876543" || m.DID != "15551234567" {
		t.Errorf("numbers not normalized: %s / %s", m.Contact, m.DID)
	}
	if m := byID["102"]; !m.IsMMS || len(m.Media) != 1 {
		t.Error("MMS row not detected")
	}
	// 10:00 in UTC-5 = 15:00 UTC
	if byID["100"].Date.Hour() != 15 {
		t.Errorf("timezone conversion wrong: %v", byID["100"].Date)
	}
}

func TestGetMessagesCrossDedup(t *testing.T) {
	// If getSMS ever mirrors an MMS row (same date/contact/direction/body),
	// the SMS copy must be dropped to avoid double-bridging.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		switch r.FormValue("method") {
		case "getSMS":
			w.Write([]byte(`{"status":"success","sms":[
				{"id":"200","date":"2026-07-20 11:00:00","type":"1","did":"5551234567","contact":"5559876543","message":"same+text"},
				{"id":"201","date":"2026-07-20 11:30:00","type":"1","did":"5551234567","contact":"5559876543","message":"unique"}
			]}`))
		case "getMMS":
			w.Write([]byte(`{"status":"success","sms":[
				{"id":"900","date":"2026-07-20 11:00:00","type":"1","did":"5551234567","contact":"5559876543","message":"same+text"}
			]}`))
		}
	})
	msgs, err := c.GetMessages(context.Background(), "15551234567", time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages after cross-dedup, got %d: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.ID == "200" {
			t.Error("SMS mirror of an MMS row should have been dropped")
		}
	}
}

func TestGetPhonebook(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		if r.FormValue("method") != "getPhonebook" {
			t.Errorf("expected getPhonebook, got %s", r.FormValue("method"))
		}
		w.Write([]byte(`{"status":"success","phonebooks":[
			{"phonebook":"11","speed_dial":"","name":"Jane Doe","number":"5559876543","callerid":"","note":""},
			{"phonebook":12,"speed_dial":"01","name":"Desk","number":"100","callerid":"","note":"internal"}
		]}`))
	})
	entries, err := c.GetPhonebook(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Name != "Jane Doe" || entries[0].Number != "15559876543" || entries[0].ID != "11" {
		t.Errorf("entry parsed wrong: %+v", entries[0])
	}
}

func TestGetPhonebookEmpty(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"no_phonebook"}`))
	})
	entries, err := c.GetPhonebook(context.Background())
	if err != nil {
		t.Fatalf("no_phonebook should not be an error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty phonebook, got %d", len(entries))
	}
}

func TestSetContactNameUpdatesExisting(t *testing.T) {
	var gotMethod, gotID, gotName string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		switch r.FormValue("method") {
		case "getPhonebook":
			w.Write([]byte(`{"status":"success","phonebooks":[
				{"phonebook":"42","speed_dial":"","name":"Old Name","number":"5559876543","callerid":"","note":"keep me"}
			]}`))
		case "setPhonebook":
			gotMethod = "setPhonebook"
			gotID = r.FormValue("phonebook")
			gotName = r.FormValue("name")
			if r.FormValue("note") != "keep me" {
				t.Errorf("note not preserved: %q", r.FormValue("note"))
			}
			w.Write([]byte(`{"status":"success"}`))
		default:
			t.Errorf("unexpected method %s", r.FormValue("method"))
			w.Write([]byte(`{"status":"success"}`))
		}
	})
	entry, err := c.SetContactName(context.Background(), "15559876543", "New Name")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "setPhonebook" || gotID != "42" || gotName != "New Name" {
		t.Errorf("update path wrong: %s/%s/%s", gotMethod, gotID, gotName)
	}
	if entry.Name != "New Name" {
		t.Errorf("returned entry not updated: %+v", entry)
	}
}

func TestSetContactNameAddsNew(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		switch r.FormValue("method") {
		case "getPhonebook":
			w.Write([]byte(`{"status":"no_phonebook"}`))
		case "addPhonebook":
			if r.FormValue("number") != "5559876543" {
				t.Errorf("number should be 10-digit for the API: %q", r.FormValue("number"))
			}
			w.Write([]byte(`{"status":"success","phonebook":77}`))
		default:
			t.Errorf("unexpected method %s", r.FormValue("method"))
			w.Write([]byte(`{"status":"success"}`))
		}
	})
	entry, err := c.SetContactName(context.Background(), "15559876543", "Jane")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != "77" || entry.Name != "Jane" || entry.Number != "15559876543" {
		t.Errorf("add path returned wrong entry: %+v", entry)
	}
}

func TestEmptyStatusIsNotError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"no_sms"}`))
	})
	msgs, err := c.GetMessages(context.Background(), "15551234567", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("no_sms should not be an error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected empty result, got %d", len(msgs))
	}
}

func TestAuthErrorDetection(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"invalid_credentials"}`))
	})
	_, err := c.GetSMSDIDs(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsAuthError(err) {
		t.Errorf("invalid_credentials should be an auth error: %v", err)
	}
}

func TestSendSMS(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		if r.FormValue("did") != "5551234567" || r.FormValue("dst") != "5559876543" {
			t.Errorf("bad did/dst: %s/%s", r.FormValue("did"), r.FormValue("dst"))
		}
		w.Write([]byte(`{"status":"success","sms":4442}`))
	})
	id, err := c.SendSMS(context.Background(), "15551234567", "15559876543", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if id != "4442" {
		t.Errorf("id = %q, want 4442", id)
	}
}

func TestSendMMSStringID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		if !strings.HasPrefix(r.FormValue("media1"), "data:image/png;base64,") {
			t.Errorf("media1 not a data URI: %.40s", r.FormValue("media1"))
		}
		w.Write([]byte(`{"status":"success","mms":"9001"}`))
	})
	id, err := c.SendMMS(context.Background(), "15551234567", "15559876543", "pic", []MediaUpload{{Data: []byte{1, 2, 3}, Mime: "image/png"}})
	if err != nil {
		t.Fatal(err)
	}
	if id != "9001" {
		t.Errorf("id = %q, want 9001", id)
	}
}

func TestGetDIDsInfoFiltering(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","dids":[
			{"did":"5551234567","description":"Main","sms_available":"1","sms_enabled":"1","mms_available":"1"},
			{"did":"5550000000","description":"Fax","sms_available":"0","sms_enabled":"0"},
			{"did":5551111111,"description":"NumberTyped","sms_available":1,"sms_enabled":1}
		]}`))
	})
	dids, err := c.GetSMSDIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dids) != 2 {
		t.Fatalf("expected 2 SMS DIDs, got %d", len(dids))
	}
	if dids[0].Number != "15551234567" || !dids[0].MMSEnabled {
		t.Errorf("first DID parsed wrong: %+v", dids[0])
	}
	if dids[1].Number != "15551111111" {
		t.Errorf("number-typed JSON fields not handled: %+v", dids[1])
	}
}

func TestGetMMSMedia(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		if r.FormValue("media_as_array") != "1" {
			t.Error("expected media_as_array=1")
		}
		w.Write([]byte(`{"status":"success","media":["https://voip.ms/media.php?x=1","https://voip.ms/media.php?x=2"]}`))
	})
	media, err := c.GetMMSMedia(context.Background(), "102")
	if err != nil {
		t.Fatal(err)
	}
	if len(media) != 2 {
		t.Errorf("expected 2 media URLs, got %d", len(media))
	}
}
