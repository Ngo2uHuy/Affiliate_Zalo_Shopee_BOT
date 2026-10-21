package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postConvert(s *Server, body, ip string) (*httptest.ResponseRecorder, map[string]any) {
	r := httptest.NewRequest("POST", "/api/convert", strings.NewReader(body))
	r.Header.Set("CF-Connecting-IP", ip)
	w := httptest.NewRecorder()
	s.webConvert(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestWebConvertDemoCleansLink(t *testing.T) {
	s := NewServer(Config{ShopeeMode: "demo"})
	w, out := postConvert(s, `{"link":"mua cái này https://shopee.vn/Ao-i.1.2?utm_source=x nha"}`, "1.1.1.1")
	if w.Code != http.StatusOK || out["link"] != "https://shopee.vn/product/1/2" || out["demo"] != true {
		t.Fatalf("%d %v", w.Code, out)
	}
}

func TestWebConvertRejectsBadInput(t *testing.T) {
	s := NewServer(Config{ShopeeMode: "demo"})
	for _, body := range []string{`{"link":"https://evil.com/x"}`, `{"link":"http://shopee.vn/x"}`, `not json`} {
		if w, out := postConvert(s, body, "1.1.1.1"); w.Code != http.StatusBadRequest || out["error"] == "" {
			t.Errorf("%s -> %d %v", body, w.Code, out)
		}
	}
}

func TestWebConvertLiveUsesWebSubID(t *testing.T) {
	shopee := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `subIds: [\"web\"]`) {
			t.Errorf("missing web subId: %s", body)
		}
		_, _ = w.Write([]byte(`{"data":{"generateShortLink":{"shortLink":"https://s.shopee.vn/WEB"}}}`))
	}))
	defer shopee.Close()
	s := NewServer(Config{ShopeeMode: "live", ShopeeAppID: "app", ShopeeSecret: "secret"})
	s.shopeeURL = shopee.URL
	w, out := postConvert(s, `{"link":"https://shopee.vn/product/1/2"}`, "1.1.1.1")
	if w.Code != http.StatusOK || out["link"] != "https://s.shopee.vn/WEB" || out["demo"] != false {
		t.Fatalf("%d %v", w.Code, out)
	}
}

func TestWebConvertFacebookSourceSubID(t *testing.T) {
	for source, want := range map[string]string{"facebook": "web_facebook", "evil\"]": "web"} {
		shopee := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `subIds: [\"`+want+`\"]`) {
				t.Errorf("source %q: want subId %s: %s", source, want, body)
			}
			_, _ = w.Write([]byte(`{"data":{"generateShortLink":{"shortLink":"https://s.shopee.vn/WEB"}}}`))
		}))
		s := NewServer(Config{ShopeeMode: "live", ShopeeAppID: "app", ShopeeSecret: "secret"})
		s.shopeeURL = shopee.URL
		src, _ := json.Marshal(source)
		if w, out := postConvert(s, `{"link":"https://shopee.vn/product/1/2","source":`+string(src)+`}`, "1.1.1.1"); w.Code != http.StatusOK {
			t.Errorf("source %q: %d %v", source, w.Code, out)
		}
		shopee.Close()
	}
}

func TestWebConvertShopeeErrorHidesDetails(t *testing.T) {
	shopee := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"internal secret detail"}]}`))
	}))
	defer shopee.Close()
	s := NewServer(Config{ShopeeMode: "live", ShopeeAppID: "app", ShopeeSecret: "secret"})
	s.shopeeURL = shopee.URL
	w, _ := postConvert(s, `{"link":"https://shopee.vn/product/1/2"}`, "1.1.1.1")
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "secret detail") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestWebRateLimitPerIP(t *testing.T) {
	s := NewServer(Config{ShopeeMode: "demo"})
	body := `{"link":"https://shopee.vn/product/1/2"}`
	for i := 0; i < webRateLimit; i++ {
		if w, _ := postConvert(s, body, "2.2.2.2"); w.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	if w, _ := postConvert(s, body, "2.2.2.2"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over limit: %d", w.Code)
	}
	if w, _ := postConvert(s, body, "3.3.3.3"); w.Code != http.StatusOK {
		t.Fatalf("other IP blocked: %d", w.Code)
	}
	var l webLimiter
	start := time.Now()
	for i := 0; i < webRateLimit; i++ {
		l.allow("x", start)
	}
	if !l.allow("x", start.Add(webRateWindow)) {
		t.Fatal("limit not reset after window")
	}
}

func TestWebIndexAndVouchers(t *testing.T) {
	s := NewServer(Config{ShopeeMode: "demo"})
	w := httptest.NewRecorder()
	s.webIndex(w, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(w.Body.String(), "/api/convert") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("index page not served")
	}
	w = httptest.NewRecorder()
	s.webVouchers(w, httptest.NewRequest("GET", "/api/vouchers", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"vouchers":[]`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
