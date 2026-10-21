package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestExperimentalRedirect(t *testing.T) {
	s := NewServer(Config{ExperimentalBaseURL: "https://bot.example"})
	link, err := s.createExperimentalLink("https://s.shopee.vn/abc?affiliate_id=123")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /r/{id}", s.experimentalRedirect)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", link, nil))
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d", w.Code)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "s.shopee.vn" || u.Path != "/abc" || u.Query().Get("affiliate_id") != "123" || u.Query().Get("utm_source") != "facebook" || !strings.HasPrefix(u.Query().Get("fbclid"), "experimental_") {
		t.Fatalf("unexpected target: %s", u)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("redirect must not be cached")
	}
	id := strings.TrimPrefix(link, "https://bot.example/r/")
	if decoded, err := base64.RawURLEncoding.DecodeString(id); err != nil || len(id) != 12 || len(decoded) != 9 {
		t.Fatalf("expected 12-character URL-safe ID: %q", id)
	}
	for _, ua := range []string{"Mozilla/5.0 (Linux; Android 14)", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0)"} {
		req := httptest.NewRequest("GET", link, nil)
		req.Header.Set("User-Agent", ua)
		page := httptest.NewRecorder()
		mux.ServeHTTP(page, req)
		body := page.Body.String()
		hash := sha256.Sum256([]byte(autoOpenScript))
		if !strings.Contains(page.Header().Get("Content-Security-Policy"), "'sha256-"+base64.StdEncoding.EncodeToString(hash[:])+"'") || !strings.Contains(body, "<script>"+autoOpenScript+"</script>") {
			t.Fatal("auto-open script missing or blocked by CSP")
		}
		if page.Code != http.StatusOK || !strings.Contains(body, "Mở ứng dụng Shopee") || !strings.Contains(body, "Tiếp tục trên web") || strings.Contains(body, "ZgotmplZ") {
			t.Fatalf("invalid mobile landing: %d %s", page.Code, body)
		}
		if strings.Contains(ua, "Android") != strings.Contains(body, "intent://reactPath?") || strings.Contains(ua, "iPhone") != strings.Contains(body, `href="shopeevn://reactPath?`) {
			t.Fatal("incorrect platform link")
		}
		if !strings.Contains(body, "affiliate_id=123") || !strings.Contains(body, "affiliate_id%3D123") {
			t.Fatal("affiliate parameter lost")
		}
	}
	s.redirects.links[id] = experimentalLink{target: u.String(), expires: time.Now().Add(-time.Second)}
	for _, path := range []string{link, "https://bot.example/r/missing?target=https://evil.example"} {
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("expired/unknown link: %d", w.Code)
		}
	}
}

func TestAndroidShopeeIntent(t *testing.T) {
	target := "https://s.shopee.vn/abc?x=1&fbclid=experimental_test#section"
	got := androidShopeeIntent(target)
	want := "intent://reactPath?navigate_url=" + url.QueryEscape(target) + "&path=shopee%2FTRANSFER_PAGE&tab=buy&use_deeplink=1&version=1#Intent;scheme=shopeevn;package=com.shopee.vn;"
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "S.browser_fallback_url="+url.QueryEscape(target)+";end") {
		t.Fatal(got)
	}
	app, err := url.Parse(shopeeAppURL(target))
	if err != nil || app.Scheme != "shopeevn" || app.Host != "reactPath" || app.Query().Get("navigate_url") != target {
		t.Fatalf("app URL = %s", app)
	}
}

func TestExperimentalLinkValidationAndDisabled(t *testing.T) {
	s := NewServer(Config{})
	const target = "https://shopee.vn/product/1/2"
	if got := s.experimentalReplyLink(target); got != target {
		t.Fatal(got)
	}
	s.redirects.baseURL = "https://current.trycloudflare.com"
	got := s.experimentalReplyLink(target)
	if !strings.HasPrefix(got, "https://current.trycloudflare.com/r/") {
		t.Fatal(got)
	}
	s.cfg.ExperimentalBaseURL = "https://stale.trycloudflare.com"
	if got := s.experimentalBaseURL(); got != s.redirects.baseURL {
		t.Fatal(got)
	}
	for _, target := range []string{"https://evil.example", "https://shopee.vn.evil.example/", "http://shopee.vn/"} {
		if _, err := s.createExperimentalLink(target); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
}

func TestShortRedirectLinkKeepsTargetUnchanged(t *testing.T) {
	s := NewServer(Config{})
	const target = "https://shopee.vn/voucher/details?evcode=QQ%3D%3D&promotionId=1"
	if got := s.shortRedirectLink(target); got != target {
		t.Fatal(got)
	}
	s.redirects.baseURL = "https://bot.example"
	link := s.shortRedirectLink(target)
	entry, ok := s.redirects.links[strings.TrimPrefix(link, "https://bot.example/r/")]
	if !ok || entry.target != target {
		t.Fatalf("link %s stored %+v", link, entry)
	}
}
