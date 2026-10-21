package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type experimentalLink struct {
	target               string
	expires              time.Time
	preserveProviderFlow bool
}

type experimentalLinks struct {
	mu      sync.Mutex
	baseURL string // current Quick Tunnel origin; protected by mu
	links   map[string]experimentalLink
}

func (s *Server) experimentalBaseURL() string {
	s.redirects.mu.Lock()
	defer s.redirects.mu.Unlock()
	if s.redirects.baseURL != "" {
		return s.redirects.baseURL
	}
	return s.cfg.ExperimentalBaseURL
}

// Experimental parameters are synthetic; they do not establish voucher eligibility.
func (s *Server) createExperimentalLink(target string) (string, error) {
	return s.createRedirect(target, true)
}

// shortRedirectLink keeps long Shopee links (such as voucher pages) readable in chat, without test parameters.
func (s *Server) shortRedirectLink(target string) string {
	if s.experimentalBaseURL() == "" {
		return target
	}
	link, err := s.createRedirect(target, false)
	if err != nil {
		log.Printf("Không tạo được link rút gọn: %v", err)
		return target
	}
	return link
}

func (s *Server) createRedirect(target string, facebookParams bool) (string, error) {
	baseURL := s.experimentalBaseURL()
	if baseURL == "" {
		return "", errors.New("experimental base URL unavailable")
	}
	if !isShopeeURL(target) && !(isAffipadURL(target) && !facebookParams) {
		return "", errors.New("invalid Shopee target")
	}
	var random [9]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(random[:])
	u, _ := url.Parse(target)
	if facebookParams {
		q := u.Query()
		q.Set("utm_source", "facebook")
		q.Set("utm_medium", "social")
		q.Set("fbclid", "experimental_"+id)
		u.RawQuery = q.Encode()
	}
	s.redirects.mu.Lock()
	defer s.redirects.mu.Unlock()
	if s.redirects.links == nil {
		s.redirects.links = make(map[string]experimentalLink)
	}
	now := time.Now()
	for key, entry := range s.redirects.links {
		if !entry.expires.After(now) {
			delete(s.redirects.links, key)
		}
	}
	if len(s.redirects.links) >= 10000 {
		return "", errors.New("experimental link store full")
	}
	if _, exists := s.redirects.links[id]; exists {
		return "", errors.New("experimental link ID collision")
	}
	s.redirects.links[id] = experimentalLink{target: u.String(), expires: now.Add(24 * time.Hour), preserveProviderFlow: s.cfg.ShopeeMode == "affipad" && !facebookParams}
	return baseURL + "/r/" + id, nil
}

func (s *Server) experimentalReplyLink(target string) string {
	if s.experimentalBaseURL() == "" {
		return target
	}
	link, err := s.createExperimentalLink(target)
	if err != nil {
		log.Printf("Không tạo được link thử nghiệm: %v", err)
		return target
	}
	return link + "\n(Link thử nghiệm nguồn Facebook; chưa xác nhận áp mã, hiệu lực 24 giờ.)"
}

func (s *Server) experimentalRedirect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.redirects.mu.Lock()
	entry, ok := s.redirects.links[r.PathValue("id")]
	s.redirects.mu.Unlock()
	if !ok || !entry.expires.After(time.Now()) {
		http.NotFound(w, r)
		return
	}
	// A response Referer header cannot spoof the browser's next request.
	if entry.preserveProviderFlow || isAffipadURL(entry.target) {
		// Preserve the provider's signed redirect chain; do not turn it into an app scheme.
		http.Redirect(w, r, entry.target, http.StatusFound)
		return
	}
	ua := strings.ToLower(r.UserAgent())
	if strings.Contains(ua, "android") || strings.Contains(ua, "iphone") || strings.Contains(ua, "ipad") {
		appURL := shopeeAppURL(entry.target)
		if strings.Contains(ua, "android") {
			appURL = androidShopeeIntent(entry.target)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		scriptHash := sha256.Sum256([]byte(autoOpenScript))
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(scriptHash[:])+"'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		// Only URLs built from our validated, stored Shopee targets are trusted here.
		if err := appLanding.Execute(w, struct {
			App template.URL
			Web string
		}{template.URL(appURL), entry.target}); err != nil {
			log.Printf("Không render được trang mở Shopee: %v", err)
		}
		return
	}
	http.Redirect(w, r, entry.target, http.StatusFound)
}

// shopeeAppDeepLinkQuery mirrors Shopee's own universal-link landing page. In-app browsers such as
// Zalo ignore HTTPS app links, but hand custom schemes to the OS. The app opens navigate_url itself,
// so the affiliate link is passed unchanged.
func shopeeAppDeepLinkQuery(target string) string {
	return url.Values{
		"navigate_url": {target},
		"path":         {"shopee/TRANSFER_PAGE"},
		"tab":          {"buy"},
		"use_deeplink": {"1"},
		"version":      {"1"},
	}.Encode()
}

func shopeeAppURL(target string) string {
	return "shopeevn://reactPath?" + shopeeAppDeepLinkQuery(target)
}

func androidShopeeIntent(target string) string {
	return "intent://reactPath?" + shopeeAppDeepLinkQuery(target) + "#Intent;scheme=shopeevn;package=com.shopee.vn;S.browser_fallback_url=" + url.QueryEscape(target) + ";end"
}

// Attempt once, when visible. Do not retry on return from the app or force a web timeout.
const autoOpenScript = `(function () {
  function openApp() {
    if (document.visibilityState !== 'visible') return;
    document.removeEventListener('visibilitychange', openApp);
    try { window.location.assign(document.getElementById('open-app').href); }
    catch (_) { document.getElementById('open-status').textContent = 'Hãy bấm Mở ứng dụng Shopee để tiếp tục.'; }
  }
  if (document.visibilityState === 'visible') openApp();
  else document.addEventListener('visibilitychange', openApp);
})();`

var appLanding = template.Must(template.New("open-shopee").Parse(`<!doctype html>
<html lang="vi"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Mở sản phẩm Shopee</title><style>body{font:18px system-ui;max-width:480px;margin:60px auto;padding:24px;line-height:1.6}a{display:block;margin:20px 0;padding:14px;border-radius:10px;text-align:center;background:#ee4d2d;color:white;text-decoration:none}.web{background:#eee;color:#222}</style>
<h1>Mở sản phẩm Shopee</h1><p id="open-status">Đang thử mở Shopee… Nếu ứng dụng chưa mở, hãy bấm nút bên dưới.</p>
<a id="open-app" href="{{.App}}">Mở ứng dụng Shopee</a>
<a class="web" href="{{.Web}}">Tiếp tục trên web</a>
<p>Nếu Zalo vẫn giữ bạn trên web, chọn menu ⋯ → Mở bằng trình duyệt, rồi bấm lại nút mở ứng dụng. Cần cài Shopee trên điện thoại.</p><script>` + autoOpenScript + `</script></html>`))
