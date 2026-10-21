package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFindShopeeURL(t *testing.T) {
	cases := []struct{ input, want string }{
		{"@Bot mua https://s.shopee.vn/abc nha", "https://s.shopee.vn/abc"},
		{"https://shopee.vn/product/123/456).", "https://shopee.vn/product/123/456"},
		{"https://evil-shopee.vn/x", ""},
		{"https://shopee.vn.evil.com/x", ""},
		{"http://shopee.vn/abc", ""},
		{"https://user@shopee.vn/abc", ""},
		{"https://shopee.vn:8443/abc", ""},
		{"https://example.com https://vn.shp.ee/x", "https://vn.shp.ee/x"},
		{"không có link", ""},
	}
	for _, tt := range cases {
		if got := findShopeeURL(tt.input); got != tt.want {
			t.Errorf("%q -> %q; want %q", tt.input, got, tt.want)
		}
	}
}

func TestProductID(t *testing.T) {
	cases := []struct{ input, shop, item string }{
		{"https://shopee.vn/Ao-thun-i.0123.456?sp_atk=x", "123", "456"},
		{"https://shopee.vn/product/11/22/", "11", "22"},
		{"https://shopee.vn/opaanlp/11/22?utm_source=other", "11", "22"},
		{"https://shopee.vn/product/0/22", "", ""},
		{"https://shopee.vn/shop-name", "", ""},
		{"https://evil.com/product/11/22", "", ""},
	}
	for _, tt := range cases {
		shop, item, _ := productID(tt.input)
		if shop != tt.shop || item != tt.item {
			t.Errorf("%q -> %s/%s; want %s/%s", tt.input, shop, item, tt.shop, tt.item)
		}
	}
}

func TestOriginURLDropsTrackingParams(t *testing.T) {
	s := NewServer(Config{})
	got := s.originURL(context.Background(), "https://shopee.vn/Ao-i.1.2?utm_source=an_other&smtt=9")
	if got != "https://shopee.vn/product/1/2" {
		t.Fatalf("got %q", got)
	}
	if got := s.originURL(context.Background(), "https://shopee.vn/m/ma-giam-gia"); got != "https://shopee.vn/m/ma-giam-gia" {
		t.Fatalf("non-product link changed: %q", got)
	}
}

func TestRedirectStaysOnShopee(t *testing.T) {
	ok, _ := http.NewRequest(http.MethodHead, "https://shopee.vn/product/1/2", nil)
	bad, _ := http.NewRequest(http.MethodHead, "http://169.254.169.254/latest", nil)
	if shopeeOnlyRedirect(ok, nil) != nil {
		t.Error("Shopee redirect rejected")
	}
	if shopeeOnlyRedirect(bad, nil) == nil {
		t.Error("redirect outside Shopee accepted")
	}
}

func event(id, chatType, text string, isBot bool) string {
	var e Event
	e.EventName = "message.text.received"
	e.Message.MessageID = id
	e.Message.Text = text
	e.Message.From.IsBot = isBot
	e.Message.Chat.ID = "chat_1"
	e.Message.Chat.ChatType = chatType
	b, _ := json.Marshal(e)
	if chatType == "GROUP" { // documented wrapper; private events use the bare form Zalo posts
		b, _ = json.Marshal(map[string]any{"ok": true, "result": e})
	}
	return string(b)
}

func TestWebhookFiltersAndDedup(t *testing.T) {
	s := NewServer(Config{WebhookSecret: "12345678", BotToken: "1:a", ShopeeMode: "demo"})
	call := func(secret, payload string) int {
		r := httptest.NewRequest("POST", "/webhook", strings.NewReader(payload))
		r.Header.Set("X-Bot-Api-Secret-Token", secret)
		w := httptest.NewRecorder()
		s.webhook(w, r)
		return w.Code
	}
	link := event("m1", "PRIVATE", "https://shopee.vn/product/1/2", false)
	if got := call("wrong", link); got != http.StatusForbidden {
		t.Fatalf("wrong secret: %d", got)
	}
	for _, payload := range []string{link, link, // duplicate is dropped
		event("m2", "GROUP", "chào cả nhà", false),                    // group chat without link
		event("m3", "PRIVATE", "https://shopee.vn/product/1/2", true), // bot's own message
		`{"ok":true,"result":{"event_name":"message.image.received"}}`,
		`not json`} {
		if got := call("12345678", payload); got != http.StatusOK {
			t.Fatalf("payload %s: %d", payload, got)
		}
	}
	call("12345678", event("m4", "PRIVATE", "xin chào", false))
	call("12345678", event("m5", "GROUP", "@Bot https://s.shopee.vn/x", false))
	call("12345678", event("m6", "GROUP", "@Bot của Nguyễn mã giảm giá 2", false))
	if got := len(s.jobs); got != 4 {
		t.Fatalf("want 4 queued jobs, got %d", got)
	}
	for i := 0; i < 3; i++ {
		<-s.jobs
	}
	if job := <-s.jobs; job.Page != 2 || job.Link != "" {
		t.Fatalf("voucher job: %+v", job)
	}

	// A pasted link with a preview card arrives as a non-text event with an undocumented link field.
	linkCard := `{"event_name":"message.link.received","message":{"message_id":"m7","from":{"is_bot":false},"chat":{"id":"chat_1","chat_type":"PRIVATE"},"attachment":{"title":"iPhone","url":"https://s.shopee.vn/8AWsALiHRG"}}}`
	call("12345678", linkCard)
	call("12345678", `{"event_name":"message.sticker.received","message":{"message_id":"m8","from":{"is_bot":false},"chat":{"id":"chat_1","chat_type":"PRIVATE"},"url":"https://zalo.me/sticker"}}`)
	if got := len(s.jobs); got != 1 {
		t.Fatalf("want 1 queued link-card job, got %d", got)
	}
	if job := <-s.jobs; job.ID != "m7" || job.Link != "https://s.shopee.vn/8AWsALiHRG" {
		t.Fatalf("link-card job: %+v", job)
	}
}

func TestConvertAndSendViaMockServers(t *testing.T) {
	var shopeeCalls atomic.Int32
	var sent string
	shopee := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shopeeCalls.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SHA256 Credential=app, Timestamp=") {
			t.Error("missing Shopee signature")
		}
		body, _ := io.ReadAll(r.Body)
		var p map[string]string
		if err := json.Unmarshal(body, &p); err != nil || !strings.Contains(p["query"], `originUrl: "https://shopee.vn/product/1/2"`) {
			t.Errorf("invalid graphql body: %s", body)
		}
		_, _ = w.Write([]byte(`{"data":{"generateShortLink":{"shortLink":"https://s.shopee.vn/TEST"}}}`))
	}))
	defer shopee.Close()
	zalo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot1:abc/sendMessage" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var p map[string]string
		_ = json.NewDecoder(r.Body).Decode(&p)
		sent = p["text"]
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":"m1"}}`))
	}))
	defer zalo.Close()
	s := NewServer(Config{BotToken: "1:abc", ShopeeMode: "live", ShopeeAppID: "app", ShopeeSecret: "secret"})
	s.shopeeURL, s.zaloBase = shopee.URL, zalo.URL
	if err := s.process(context.Background(), Job{ID: "m1", ChatID: "chat1", Link: "https://shopee.vn/x-i.1.2?utm_source=x"}); err != nil {
		t.Fatal(err)
	}
	if shopeeCalls.Load() != 1 || !strings.Contains(sent, "https://s.shopee.vn/TEST") {
		t.Fatalf("calls=%d sent=%q", shopeeCalls.Load(), sent)
	}
}

func TestAnswerCommentsLinkOnDealPost(t *testing.T) {
	shopee := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"generateShortLink":{"shortLink":"https://s.shopee.vn/TEST"}}}`))
	}))
	defer shopee.Close()
	var comment url.Values
	fb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1_2/comments" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = r.ParseForm()
		comment = r.PostForm
		_, _ = w.Write([]byte(`{"id":"1_2_3"}`))
	}))
	defer fb.Close()
	notes := filepath.Join(t.TempDir(), "deal_vouchers.txt")
	_ = os.WriteFile(notes, []byte("🟣 Mã Instagram:\n- Giảm 22% tối đa 300k\n"), 0o600)
	s := NewServer(Config{ShopeeMode: "live", ShopeeAppID: "app", ShopeeSecret: "secret",
		DealPostURL: "https://www.facebook.com/share/p/abc/", DealPostID: "1_2", PageToken: "tok"})
	s.shopeeURL, s.graphBase, s.dealNotes = shopee.URL, fb.URL, notes
	got := s.answer(context.Background(), Job{Link: "https://shopee.vn/product/1/2"})
	if comment.Get("message") != "https://s.shopee.vn/TEST" || comment.Get("access_token") != "tok" {
		t.Errorf("comment = %v", comment)
	}
	for _, want := range []string{"(cmt mới nhất):\nhttps://www.facebook.com/share/p/abc/", "Mã Instagram", "Giảm 22%"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer missing %q: %s", want, got)
		}
	}

	fb.Close() // comment fails: still point to the post, without claiming the newest comment
	got = s.answer(context.Background(), Job{Link: "https://shopee.vn/product/1/2"})
	if strings.Contains(got, "cmt mới nhất") || !strings.Contains(got, "https://www.facebook.com/share/p/abc/") {
		t.Errorf("fallback answer: %s", got)
	}
}

func TestShopeeErrorAndForeignShortLink(t *testing.T) {
	for _, body := range []string{`{"errors":[{"message":"invalid originUrl"}]}`,
		`{"data":{"generateShortLink":{"shortLink":"https://evil.com/x"}}}`} {
		shopee := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		s := NewServer(Config{ShopeeMode: "live", ShopeeAppID: "app", ShopeeSecret: "secret"})
		s.shopeeURL = shopee.URL
		if _, err := s.convert(context.Background(), "https://shopee.vn/product/1/2", "zalo_bot"); err == nil {
			t.Errorf("accepted %s", body)
		}
		shopee.Close()
	}
}

func TestZaloErrorsHideTokenAndRetryOnlyTemporary(t *testing.T) {
	var calls atomic.Int32
	zalo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"bad"}`))
	}))
	s := NewServer(Config{BotToken: "1:SECRET"})
	s.zaloBase = zalo.URL
	if err := s.sendZalo(context.Background(), "c", "x"); err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	zalo.Close()
	err := s.sendZalo(context.Background(), "c", "x") // server closed: connection error
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks token or is nil: %v", err)
	}
}

func TestLoadDotEnvKeepsRealEnvironment(t *testing.T) {
	path := t.TempDir() + "/.env"
	_ = os.WriteFile(path, []byte("\xef\xbb\xbf# comment\nBOT_TEST_A = 1\nBOT_TEST_B=x=y\n"), 0o600)
	t.Setenv("BOT_TEST_A", "from-env")
	t.Setenv("BOT_TEST_B", "") // registers cleanup; unset so the file value applies
	_ = os.Unsetenv("BOT_TEST_B")
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if a, b := os.Getenv("BOT_TEST_A"), os.Getenv("BOT_TEST_B"); a != "from-env" || b != "x=y" {
		t.Errorf("A=%q B=%q", a, b)
	}
	if err := os.WriteFile(path, []byte("missing equals\n"), 0o600); err != nil || loadDotEnv(path) == nil {
		t.Error("invalid line accepted")
	}
}
