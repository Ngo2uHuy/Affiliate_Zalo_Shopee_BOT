package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type affipadTransport func(*http.Request) (*http.Response, error)

func TestAffipadShortURLPreference(t *testing.T) {
	for _, tc := range []struct{ short, want string }{
		{"https://shp.ee/abc", "https://shp.ee/abc"},
		{"https://vn.shp.ee/abc", "https://vn.shp.ee/abc"},
		{"https://shp.ee.evil.example/abc", "https://s.afp.ad/fallback"},
		{"http://shp.ee/abc", "https://s.afp.ad/fallback"},
		{"", "https://s.afp.ad/fallback"},
	} {
		s := NewServer(Config{ShopeeMode: "affipad"})
		s.http.Transport = affipadTransport(func(*http.Request) (*http.Response, error) {
			body, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"results": []map[string]string{{"link": "https://s.afp.ad/fallback", "shortUrl": tc.short}}}})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})
		got, err := s.convertAffipad(context.Background(), "https://shopee.vn/product/1/2", "zalo_bot")
		if err != nil || got != tc.want {
			t.Fatalf("short %q: got %q, error %v", tc.short, got, err)
		}
	}
}

func (f affipadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAffipadConvertAndRedirect(t *testing.T) {
	s := NewServer(Config{ShopeeMode: "affipad", AffipadAPIKey: "test-secret", AffipadToolID: "tool1", ExperimentalBaseURL: "https://bot.example"})
	s.http.Transport = affipadTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.affipad.com/v1/convert" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatal("incorrect request")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["toolId"] != "tool1" || body["resolveRedirect"] != false || body["useShortLink"] != true || body["subId"] != "zalo_bot" || body["tracking"].(map[string]any)["preset"] != "fb_reels" {
			t.Fatal(body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"data":{"results":[{"link":"https://s.afp.ad/test?sig=abc%2Bdef"}]}}`)), Header: make(http.Header)}, nil
	})
	target, err := s.convert(context.Background(), "https://shopee.vn/product/1/2", "zalo_bot")
	if err != nil {
		t.Fatal(err)
	}
	link := s.shortRedirectLink(target)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /r/{id}", s.experimentalRedirect)
	req := httptest.NewRequest("GET", link, nil)
	req.Header.Set("User-Agent", "Android")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 302 || w.Header().Get("Location") != target {
		t.Fatalf("redirect changed: %d %s", w.Code, w.Header().Get("Location"))
	}
	if _, err := s.createExperimentalLink(target); err == nil {
		t.Fatal("must not modify AffiPad parameters")
	}
}

func TestAffipadInvalidResponses(t *testing.T) {
	for _, body := range []string{`bad json`, `{"success":false}`, `{"success":true,"data":{"results":[]}}`, `{"success":true,"data":{"results":[{"link":"https://s.afp.ad.evil.example/x"}]}}`} {
		s := NewServer(Config{ShopeeMode: "affipad"})
		s.http.Transport = affipadTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if _, err := s.convertAffipad(context.Background(), "https://shopee.vn/product/1/2", "zalo_bot"); err == nil {
			t.Fatal(body)
		}
	}
}

func TestAffipadShopeeRedirectResponse(t *testing.T) {
	s := NewServer(Config{ShopeeMode: "affipad", ExperimentalBaseURL: "https://bot.example"})
	const target = "https://s.shopee.vn/an_redir?origin_link=abc%2Bdef&signature=xyz"
	s.http.Transport = affipadTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"data":{"results":[{"link":"` + target + `"}]}}`))}, nil
	})
	got, err := s.convertAffipad(context.Background(), "https://shopee.vn/product/1/2", "zalo_bot")
	if err != nil || got != target {
		t.Fatalf("got %q, err %v", got, err)
	}
	link := s.shortRedirectLink(got)
	id := strings.TrimPrefix(link, "https://bot.example/r/")
	if s.redirects.links[id].target != target {
		t.Fatal("provider parameters changed")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /r/{id}", s.experimentalRedirect)
	for _, ua := range []string{"Android", "iPhone"} {
		req := httptest.NewRequest("GET", link, nil)
		req.Header.Set("User-Agent", ua)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusFound || w.Header().Get("Location") != target {
			t.Fatalf("provider flow intercepted for %s: status %d", ua, w.Code)
		}
	}
}
