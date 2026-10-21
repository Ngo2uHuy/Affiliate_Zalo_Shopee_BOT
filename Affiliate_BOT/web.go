package main

// Public web page: paste a Shopee link, get the affiliate link plus the voucher
// list. Served on the same port as the webhook, so the Quick Tunnel URL works too.

import (
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	webRateLimit  = 10 // conversions per IP per minute; each one calls the Shopee API
	webRateWindow = time.Minute
)

//go:embed web/index.html
var indexHTML []byte

type webLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *webLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	if len(l.hits) > 10000 {
		for k, times := range l.hits {
			if now.Sub(times[len(times)-1]) > webRateWindow {
				delete(l.hits, k)
			}
		}
	}
	recent := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if now.Sub(t) < webRateWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= webRateLimit {
		l.hits[ip] = recent
		return false
	}
	l.hits[ip] = append(recent, now)
	return true
}

// clientIP trusts CF-Connecting-IP because the server only listens on 127.0.0.1,
// so outside traffic can only arrive through the Cloudflare tunnel.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) webIndex(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(indexHTML)
}

type webConvertResult struct {
	Origin       string `json:"origin"`
	Link         string `json:"link"`
	Demo         bool   `json:"demo"`
	Review       string `json:"review,omitempty"`
	ReviewIsPost bool   `json:"review_is_post,omitempty"`
}

func (s *Server) webConvert(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(clientIP(r), time.Now()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Bạn đổi link hơi nhanh, chờ 1 phút rồi thử lại nhé."})
		return
	}
	var req struct {
		Link   string `json:"link"`
		Source string `json:"source"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Dữ liệu gửi lên không hợp lệ."})
		return
	}
	link := findShopeeURL(req.Link)
	if link == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Không thấy link Shopee hợp lệ (https://shopee.vn/... hoặc https://s.shopee.vn/...)."})
		return
	}
	res := webConvertResult{Origin: s.originURL(r.Context(), link), Demo: s.cfg.ShopeeMode == "demo"}
	res.Review, res.ReviewIsPost = s.backlinks.Target(res.Origin)
	if res.Demo {
		res.Link = res.Origin
	} else {
		// Only a fixed subId is accepted, so clients cannot write arbitrary values into Shopee reports.
		subID := "web"
		if req.Source == "facebook" {
			subID = "web_facebook"
		}
		short, err := s.convert(r.Context(), res.Origin, subID)
		if err != nil {
			log.Printf("Web: Shopee chưa chuyển đổi được link: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Shopee chưa chuyển đổi được link này. Hãy dùng link sản phẩm đầy đủ (app Shopee → Chia sẻ → Sao chép liên kết) hoặc thử lại sau."})
			return
		}
		res.Link = short
	}
	writeJSON(w, http.StatusOK, res)
}

type webVoucher struct {
	Code        string    `json:"code"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	Expires     time.Time `json:"expires"`
}

func (s *Server) webVouchers(w http.ResponseWriter, r *http.Request) {
	out := []webVoucher{}
	if s.vouchers != nil {
		seen := map[string]bool{}
		for _, v := range s.vouchers.Get(r.Context()) {
			key := strings.ToLower(v.Code)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, webVoucher{Code: v.Code, Description: v.Description, URL: v.URL, Expires: v.Expires})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"vouchers": out, "more": vouchersPageURL})
}
