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
		if r.FormValue("method") != "getMMS" {
			t.Errorf("expected getMMS, got %s", r.FormValue("method"))
		}
		if r.FormValue("all_messages") != "1" {
			t.Error("expected all_messages=1")
		}
		w.Write([]byte(`{"status":"success","sms":[
			{"id":"100","date":"2026-07-20 10:00:00","type":"1","did":"5551234567","contact":"5559876543","message":"hello+there","col_media1":null},
			{"id":"101","date":"2026-07-20 10:05:00","type":"0","did":"5551234567","contact":"5559876543","message":"reply","col_media1":null},
			{"id":"102","date":"2026-07-20 10:10:00","type":"1","did":"5551234567","contact":"5559876543","message":"","col_media1":"https://voip.ms/media.php?x=1"},
			{"id":"103","date":"2026-07-20 10:15:00","type":"1","did":"5551234567","contact":"5559876543","message":"","col_media1":null}
		]}`))
	})
	msgs, err := c.GetMessages(context.Background(), "15551234567", time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (malformed row skipped), got %d", len(msgs))
	}
	if msgs[0].Body != "hello there" {
		t.Errorf("body not URL-decoded: %q", msgs[0].Body)
	}
	if !msgs[0].Inbound || msgs[1].Inbound {
		t.Error("type field parsed wrong")
	}
	if msgs[0].Contact != "15559876543" || msgs[0].DID != "15551234567" {
		t.Errorf("numbers not normalized: %s / %s", msgs[0].Contact, msgs[0].DID)
	}
	if !msgs[2].IsMMS || len(msgs[2].Media) != 1 {
		t.Error("MMS row not detected")
	}
	// 10:00 in UTC-5 = 15:00 UTC
	if msgs[0].Date.Hour() != 15 {
		t.Errorf("timezone conversion wrong: %v", msgs[0].Date)
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
