// Zalo Bot: receives a Shopee link, converts it with the Shopee Affiliate Open API
// and replies with the affiliate short link. Standard library only.
//
// Learning MVP: the job queue and duplicate guard live in RAM, so jobs accepted
// right before a restart can be lost. See README for production notes.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	shopeeEndpoint = "https://open-api.affiliate.shopee.vn/graphql"
	zaloAPIBase    = "https://bot-api.zaloplatforms.com"
	maxBodyBytes   = 64 << 10
	maxLinkLength  = 4096 // app share links carry long JSON query strings
)

var (
	urlFinder    = regexp.MustCompile(`https?://[^\s<>"'\x{200b}]+`)
	slugProduct  = regexp.MustCompile(`-i\.(\d+)\.(\d+)/?$`)
	pathProduct  = regexp.MustCompile(`^/(?:product|opaanlp)/(\d+)/(\d+)/?$`)
	tunnelFinder = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
	tokenFormat  = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
	shopeeHosts  = map[string]bool{"shopee.vn": true, "www.shopee.vn": true, "s.shopee.vn": true,
		"u.shopee.vn": true, "shope.ee": true, "vn.shp.ee": true}
	shortHosts = map[string]bool{"s.shopee.vn": true, "u.shopee.vn": true, "shope.ee": true, "vn.shp.ee": true}
)

type Config struct {
	AffipadAPIKey       string
	AffipadToolID       string
	ExperimentalBaseURL string
	BotToken            string
	WebhookSecret       string
	ShopeeAppID         string
	ShopeeSecret        string
	ShopeeMode          string // demo or live
	Port                string
	Workers             int
	FacebookPage        string
	DealPostURL         string // Facebook post where affiliate links are commented
	DealPostID          string // "pageid_postid"; with PageToken the bot comments automatically
	PageToken           string
}

// Incoming accepts both the documented {"ok":true,"result":{...}} wrapper and the
// bare event that Zalo actually posts to webhooks.
type Incoming struct {
	Event
	Result *Event `json:"result"`
}

func (in Incoming) event() Event {
	if in.Result != nil && in.Result.EventName != "" {
		return *in.Result
	}
	return in.Event
}

type Event struct {
	EventName string `json:"event_name"`
	Message   struct {
		MessageID string `json:"message_id"`
		Text      string `json:"text"`
		From      struct {
			IsBot bool `json:"is_bot"`
		} `json:"from"`
		Chat struct {
			ID       string `json:"id"`
			ChatType string `json:"chat_type"`
		} `json:"chat"`
	} `json:"message"`
}

type Job struct {
	ID     string
	ChatID string
	Link   string // empty: private chat without a Shopee link
	Page   int    // > 0: voucher list page requested
}

type Server struct {
	redirects experimentalLinks
	cfg       Config
	http      *http.Client
	resolver  *http.Client
	jobs      chan Job
	mu        sync.Mutex
	seen      map[string]time.Time
	vouchers  *VoucherList
	backlinks *Backlinks
	limiter   webLimiter
	shopeeURL string // injectable in unit tests
	zaloBase  string // injectable in unit tests
	graphBase string // injectable in unit tests
	dealNotes string // injectable in unit tests
}

func main() {
	log.SetFlags(log.LstdFlags)
	envFile := flag.String("env", ".env", "file cấu hình")
	tunnel := flag.Bool("tunnel", false, "tự mở Cloudflare Quick Tunnel và đăng ký webhook")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Dùng: go run . [-tunnel] [-env .env]\n      go run . set-webhook https://ten-mien-cua-ban")
		flag.PrintDefaults()
	}
	flag.Parse()
	if err := loadDotEnv(*envFile); err != nil {
		log.Fatal(err)
	}
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if flag.Arg(0) == "set-webhook" {
		if flag.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		ok, err := setWebhook(context.Background(), NewServer(cfg), flag.Arg(1))
		if err != nil {
			log.Fatal(err)
		}
		if !ok {
			log.Print("Zalo đã lưu URL nhưng chưa xác minh được; hãy chạy bot rồi đăng ký lại.")
		}
		return
	}
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(cfg, *tunnel); err != nil {
		log.Fatal(err)
	}
}

func run(cfg Config, tunnel bool) error {
	s := NewServer(cfg)
	sources, err := readVoucherSources("shopee_sources.json")
	if err != nil {
		return err
	}
	s.vouchers = NewVoucherList(sources, &http.Client{Timeout: 6 * time.Second})
	if s.backlinks, err = NewBacklinks("facebook_posts.json", cfg.FacebookPage); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("POST /webhook", s.webhook)
	mux.HandleFunc("GET /{$}", s.webIndex)
	mux.HandleFunc("POST /api/convert", s.webConvert)
	mux.HandleFunc("GET /api/vouchers", s.webVouchers)
	mux.HandleFunc("GET /r/{id}", s.experimentalRedirect)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var wg sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.worker(ctx) }()
	}
	srv := &http.Server{
		Addr:              "127.0.0.1:" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      12 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Bot chạy tại http://127.0.0.1:%s (SHOPEE_MODE=%s, workers=%d). Ctrl+C để dừng.", cfg.Port, cfg.ShopeeMode, cfg.Workers)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			stop()
		}
	}()
	if tunnel {
		publicURL, err := startTunnel(ctx, cfg.Port)
		if err != nil {
			stop()
		} else {
			log.Printf("Tunnel: %s (website: %s/)", publicURL, publicURL)
			s.redirects.mu.Lock()
			s.redirects.baseURL = publicURL
			s.redirects.mu.Unlock()
			ok, werr := setWebhook(ctx, s, publicURL)
			if werr != nil {
				err = werr
				stop()
			} else if !ok {
				log.Print("Cảnh báo: Zalo chưa xác minh được webhook; kiểm tra lại cửa sổ này và thử chạy lại.")
			}
		}
		if err != nil {
			select {
			case serverErr <- err:
			default: // a listen error is already reported
			}
		}
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	wg.Wait()
	select {
	case err := <-serverErr:
		return err
	default:
		log.Print("Đã dừng bot.")
		return nil
	}
}

// loadDotEnv fills variables missing from the environment; the real environment wins.
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("không đọc được %s: %w", path, err)
	}
	for i, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s dòng %d sai dạng TEN=GIA_TRI", path, i+1)
		}
		key = strings.TrimSpace(key)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, strings.TrimSpace(value))
		}
	}
	return nil
}

func configFromEnv() (Config, error) {
	cfg := Config{
		AffipadAPIKey:       strings.TrimSpace(os.Getenv("AFFIPAD_API_KEY")),
		AffipadToolID:       strings.TrimSpace(os.Getenv("AFFIPAD_TOOL_ID")),
		ExperimentalBaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("EXPERIMENTAL_LINK_BASE_URL")), "/"),
		BotToken:            strings.TrimSpace(os.Getenv("ZALO_BOT_TOKEN")),
		WebhookSecret:       os.Getenv("ZALO_WEBHOOK_SECRET"),
		ShopeeAppID:         strings.TrimSpace(os.Getenv("SHOPEE_APP_ID")),
		ShopeeSecret:        strings.TrimSpace(os.Getenv("SHOPEE_SECRET")),
		ShopeeMode:          strings.ToLower(strings.TrimSpace(os.Getenv("SHOPEE_MODE"))),
		Port:                os.Getenv("PORT"),
		Workers:             4,
		FacebookPage:        strings.TrimSpace(os.Getenv("FACEBOOK_PAGE_URL")),
		DealPostURL:         strings.TrimSpace(os.Getenv("FACEBOOK_DEAL_POST_URL")),
		DealPostID:          strings.TrimSpace(os.Getenv("FACEBOOK_DEAL_POST_ID")),
		PageToken:           strings.TrimSpace(os.Getenv("FACEBOOK_PAGE_TOKEN")),
	}
	if cfg.ShopeeMode == "" {
		cfg.ShopeeMode = "demo"
	}
	if cfg.ExperimentalBaseURL != "" {
		u, err := url.Parse(cfg.ExperimentalBaseURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return cfg, errors.New("EXPERIMENTAL_LINK_BASE_URL phải là HTTPS origin, không có path/query")
		}
	}
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	if s := os.Getenv("WORKERS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 32 {
			return cfg, errors.New("WORKERS phải từ 1 đến 32")
		}
		cfg.Workers = n
	}
	if !tokenFormat.MatchString(cfg.BotToken) {
		return cfg, errors.New("ZALO_BOT_TOKEN thiếu hoặc sai định dạng (điền trong .env)")
	}
	if n := len(cfg.WebhookSecret); n < 8 || n > 256 {
		return cfg, errors.New("ZALO_WEBHOOK_SECRET phải dài 8–256 ký tự")
	}
	if cfg.ShopeeMode != "demo" && cfg.ShopeeMode != "live" && cfg.ShopeeMode != "affipad" {
		return cfg, errors.New("SHOPEE_MODE chỉ nhận demo, live hoặc affipad")
	}
	if cfg.ShopeeMode == "affipad" && (cfg.AffipadAPIKey == "" || cfg.AffipadToolID == "") {
		return cfg, errors.New("SHOPEE_MODE=affipad cần AFFIPAD_API_KEY và AFFIPAD_TOOL_ID")
	}
	if cfg.FacebookPage != "" && !isFacebookURL(cfg.FacebookPage) {
		return cfg, errors.New("FACEBOOK_PAGE_URL phải là link https Facebook")
	}
	if cfg.DealPostURL != "" && !isFacebookURL(cfg.DealPostURL) {
		return cfg, errors.New("FACEBOOK_DEAL_POST_URL phải là link https Facebook")
	}
	if (cfg.DealPostID == "") != (cfg.PageToken == "") {
		return cfg, errors.New("FACEBOOK_DEAL_POST_ID và FACEBOOK_PAGE_TOKEN phải điền cùng nhau")
	}
	if cfg.DealPostID != "" && (cfg.DealPostURL == "" || !postIDFormat.MatchString(cfg.DealPostID)) {
		return cfg, errors.New("FACEBOOK_DEAL_POST_ID dạng pageid_postid và cần FACEBOOK_DEAL_POST_URL")
	}
	if cfg.ShopeeMode == "live" && (cfg.ShopeeAppID == "" || cfg.ShopeeSecret == "") {
		return cfg, errors.New("SHOPEE_MODE=live cần SHOPEE_APP_ID và SHOPEE_SECRET")
	}
	return cfg, nil
}

func NewServer(cfg Config) *Server {
	return &Server{
		cfg: cfg, jobs: make(chan Job, 1024), seen: make(map[string]time.Time),
		shopeeURL: shopeeEndpoint, zaloBase: zaloAPIBase, graphBase: graphAPIBase, dealNotes: dealNotesFile,
		http:     &http.Client{Timeout: 12 * time.Second},
		resolver: &http.Client{Timeout: 6 * time.Second, CheckRedirect: shopeeOnlyRedirect},
	}
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	supplied := r.Header.Get("X-Bot-Api-Secret-Token")
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(s.cfg.WebhookSecret)) != 1 {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var update Incoming
	raw, err := io.ReadAll(r.Body)
	// Zalo's verification and unsupported events must still get 2xx, otherwise setWebhook fails.
	if err != nil || json.Unmarshal(raw, &update) != nil {
		log.Print("Bỏ qua: dữ liệu webhook không phải JSON hợp lệ.")
		writeOK(w)
		return
	}
	ev := update.event()
	if ev.EventName == "" {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		log.Printf("Sự kiện không có event_name, các trường: %v", keys)
	}
	m := ev.Message
	isText := ev.EventName == "message.text.received"
	text := m.Text
	if !isText && strings.HasPrefix(ev.EventName, "message.") {
		// A pasted link with a preview card is not delivered as a text event, and its link field is undocumented.
		text = findShopeeURLInJSON(raw)
	}
	if (!isText && text == "") || m.From.IsBot || m.Chat.ID == "" || m.MessageID == "" {
		log.Printf("Bỏ qua sự kiện %q (không phải tin của người dùng có văn bản hoặc link Shopee).", ev.EventName)
		writeOK(w)
		return
	}
	link, page := findShopeeURL(text), 0
	if link == "" {
		page = voucherPage(text)
	}
	log.Printf("Nhận tin %s (%s, %s, %d ký tự, có link Shopee: %t, lệnh mã giảm giá: %t).",
		m.MessageID, m.Chat.ChatType, ev.EventName, len(text), link != "", page > 0)
	if link == "" && page == 0 && m.Chat.ChatType == "GROUP" {
		writeOK(w) // stay quiet in groups unless someone sends a link or a command
		return
	}
	key := m.Chat.ID + ":" + m.MessageID
	now := time.Now()
	s.mu.Lock()
	if len(s.seen) > 5000 {
		for k, when := range s.seen {
			if now.Sub(when) > 15*time.Minute {
				delete(s.seen, k)
			}
		}
	}
	if _, exists := s.seen[key]; exists {
		s.mu.Unlock()
		writeOK(w)
		return
	}
	s.seen[key] = now
	s.mu.Unlock()

	select {
	case s.jobs <- Job{ID: m.MessageID, ChatID: m.Chat.ID, Link: link, Page: page}:
		writeOK(w)
	default:
		s.mu.Lock()
		delete(s.seen, key)
		s.mu.Unlock()
		http.Error(w, "Busy; retry later", http.StatusServiceUnavailable)
	}
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"message":"Success"}`))
}

func (s *Server) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.jobs:
			if err := s.process(ctx, job); err != nil {
				log.Printf("Tin %s: %v", job.ID, err)
			} else {
				log.Printf("Tin %s: đã trả lời.", job.ID)
			}
		}
	}
}

func (s *Server) process(ctx context.Context, job Job) error {
	return s.sendZalo(ctx, job.ChatID, s.answer(ctx, job))
}

func (s *Server) answer(ctx context.Context, job Job) string {
	if job.Page > 0 && s.vouchers != nil {
		return listVouchers(s.vouchers.Get(ctx), job.Page)
	}
	link := job.Link
	if link == "" {
		return "Xin chào! Gửi link sản phẩm Shopee (https://shopee.vn/... hoặc https://s.shopee.vn/...) để nhận link mua hàng.\n" +
			"Gửi \"mã giảm giá\" để xem danh sách mã."
	}
	origin := s.originURL(ctx, link)
	backlink := ""
	if line := s.backlinks.Line(origin); line != "" {
		backlink = "\n" + line
	}
	vouchers := ""
	if s.vouchers != nil {
		vouchers = productVoucherLines(s.vouchers.Get(ctx), s.shortRedirectLink, 5)
	}
	if s.cfg.ShopeeMode == "demo" {
		origin = s.experimentalReplyLink(origin)
		return "✅ BOT DEMO đã nhận link Shopee:\n" + origin + "\n\n" + vouchers + backlink
	}
	short, err := s.convert(ctx, origin, "zalo_bot")
	if err != nil {
		log.Printf("Shopee chưa chuyển đổi được link: %v", err)
		return "⚠️ Shopee chưa chuyển đổi được link này. Hãy gửi link sản phẩm đầy đủ (mở sản phẩm trong app Shopee → Chia sẻ → Sao chép liên kết) hoặc thử lại sau."
	}
	deal := " Voucher Facebook/Instagram không được bảo đảm; hãy kiểm tra ưu đãi ở trang thanh toán.\n"
	if lines := s.dealLines(ctx, short); lines != "" {
		deal = "\n\n" + lines + "\n\n"
	}
	if s.cfg.ShopeeMode == "affipad" {
		if !isProviderShortURL(short) {
			short = s.shortRedirectLink(short)
		}
	} else {
		short = s.experimentalReplyLink(short)
	}
	return "✅ Link mua hàng của bạn:\n" + short + "\n\n" + vouchers +
		"Đây là link tiếp thị liên kết, người tạo bot có thể nhận hoa hồng từ đơn hợp lệ." + deal +
		"Xem mã giảm giá: @bot mã giảm giá" + backlink
}

func findShopeeURL(text string) string {
	for _, token := range urlFinder.FindAllString(text, -1) {
		token = strings.TrimRight(token, ".,;:!?)]}")
		if isShopeeURL(token) {
			return token
		}
	}
	return ""
}

// findShopeeURLInJSON searches every string value, since the field holding a link message's URL is undocumented.
func findShopeeURLInJSON(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var walk func(v any) string
	walk = func(v any) string {
		switch v := v.(type) {
		case string:
			return findShopeeURL(v)
		case map[string]any:
			for _, child := range v {
				if link := walk(child); link != "" {
					return link
				}
			}
		case []any:
			for _, child := range v {
				if link := walk(child); link != "" {
					return link
				}
			}
		}
		return ""
	}
	return walk(v)
}

func isShopeeURL(raw string) bool {
	if len(raw) > maxLinkLength {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") &&
		shopeeHosts[strings.ToLower(u.Hostname())]
}

// productID returns shop and item IDs from a full Shopee product link.
func productID(raw string) (string, string, bool) {
	if !isShopeeURL(raw) {
		return "", "", false
	}
	u, _ := url.Parse(raw)
	m := slugProduct.FindStringSubmatch(u.Path)
	if m == nil {
		m = pathProduct.FindStringSubmatch(u.Path)
	}
	if m == nil {
		return "", "", false
	}
	shop, err1 := strconv.ParseUint(m[1], 10, 64)
	item, err2 := strconv.ParseUint(m[2], 10, 64)
	if err1 != nil || err2 != nil || shop == 0 || item == 0 {
		return "", "", false
	}
	return strconv.FormatUint(shop, 10), strconv.FormatUint(item, 10), true
}

// originURL rebuilds a clean product link so another affiliate's tracking
// parameters in the customer's link are never passed on.
func (s *Server) originURL(ctx context.Context, link string) string {
	target := link
	if u, err := url.Parse(link); err == nil && shortHosts[strings.ToLower(u.Hostname())] {
		if resolved := s.resolve(ctx, link); resolved != "" {
			target = resolved
		}
	}
	if shop, item, ok := productID(target); ok {
		return "https://shopee.vn/product/" + shop + "/" + item
	}
	return link
}

func (s *Server) resolve(ctx context.Context, link string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, link, nil)
	if err != nil {
		return ""
	}
	resp, err := s.resolver.Do(req)
	if err != nil {
		return ""
	}
	resp.Body.Close()
	return resp.Request.URL.String()
}

// shopeeOnlyRedirect blocks redirects leaving Shopee, so user links cannot make the bot fetch arbitrary hosts.
func shopeeOnlyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("quá nhiều redirect")
	}
	if !isShopeeURL(req.URL.String()) {
		return errors.New("redirect ra ngoài Shopee")
	}
	return nil
}

func (s *Server) convert(ctx context.Context, originURL, subID string) (string, error) {
	if s.cfg.ShopeeMode == "affipad" {
		return s.convertAffipad(ctx, originURL, subID)
	}
	// JSON string escaping is valid GraphQL string syntax, so the URL cannot break the query.
	jsonURL, _ := json.Marshal(originURL)
	gql := fmt.Sprintf(`mutation { generateShortLink(input: { originUrl: %s, subIds: [%q] }) { shortLink } }`, jsonURL, subID)
	body, _ := json.Marshal(map[string]string{"query": gql})
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha256.Sum256([]byte(s.cfg.ShopeeAppID + timestamp + string(body) + s.cfg.ShopeeSecret))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.shopeeURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("SHA256 Credential=%s, Timestamp=%s, Signature=%s",
		s.cfg.ShopeeAppID, timestamp, hex.EncodeToString(sum[:])))
	buf, status, err := s.do(req)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("Shopee HTTP %d", status)
	}
	var result struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			GenerateShortLink struct {
				ShortLink string `json:"shortLink"`
			} `json:"generateShortLink"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf, &result); err != nil {
		return "", errors.New("Shopee trả JSON không hợp lệ")
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("Shopee GraphQL lỗi: %s", result.Errors[0].Message)
	}
	link := result.Data.GenerateShortLink.ShortLink
	if !isShopeeURL(link) {
		return "", errors.New("shortLink trả về không phải link Shopee HTTPS")
	}
	return link, nil
}

func (s *Server) sendZalo(ctx context.Context, chatID, text string) error {
	body, _ := json.Marshal(map[string]string{"chat_id": chatID, "text": text})
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		_, err := s.zalo(ctx, "sendMessage", body)
		var status httpStatusError
		// Retry only rejections Zalo marks as temporary; a timeout may already have delivered the message.
		if err == nil || !errors.As(err, &status) || (status != http.StatusTooManyRequests && status < 500) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

type httpStatusError int

func (e httpStatusError) Error() string { return fmt.Sprintf("Zalo HTTP %d", int(e)) }

func (s *Server) zalo(ctx context.Context, method string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.zaloBase+"/bot"+s.cfg.BotToken+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("không tạo được request Zalo")
	}
	req.Header.Set("Content-Type", "application/json")
	buf, status, err := s.do(req)
	if err != nil {
		return nil, err
	}
	var result struct {
		OK          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if json.Unmarshal(buf, &result) == nil && !result.OK && result.ErrorCode != 0 {
		return nil, fmt.Errorf("Zalo %s lỗi %d %s", method, result.ErrorCode, result.Description)
	}
	if status < 200 || status >= 300 {
		return nil, httpStatusError(status)
	}
	if !result.OK {
		return nil, fmt.Errorf("Zalo %s trả ok=false", method)
	}
	return result.Result, nil
}

// do drops the request URL from network errors: Zalo URLs contain the bot token.
func (s *Server) do(req *http.Request) ([]byte, int, error) {
	resp, err := s.http.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, 0, fmt.Errorf("lỗi kết nối %s: %v", req.URL.Hostname(), urlErr.Err)
		}
		return nil, 0, errors.New("lỗi kết nối")
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("lỗi đọc phản hồi %s", req.URL.Hostname())
	}
	return buf, resp.StatusCode, nil
}

// setWebhook registers publicURL/webhook and retries while Zalo cannot reach a fresh tunnel yet.
func setWebhook(ctx context.Context, s *Server, publicURL string) (bool, error) {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return false, errors.New("URL webhook phải là HTTPS, ví dụ https://abc.trycloudflare.com")
	}
	body, _ := json.Marshal(map[string]string{"url": base + "/webhook", "secret_token": s.cfg.WebhookSecret})
	for attempt := 1; attempt <= 6; attempt++ {
		raw, err := s.zalo(ctx, "setWebhook", body)
		if err != nil {
			return false, err
		}
		var result struct {
			Verification struct {
				OK      bool   `json:"ok"`
				Outcome string `json:"outcome"`
			} `json:"verification"`
		}
		_ = json.Unmarshal(raw, &result)
		if result.Verification.OK {
			log.Printf("Đã đăng ký webhook %s/webhook (%s).", base, result.Verification.Outcome)
			return true, nil
		}
		log.Printf("Zalo chưa xác minh webhook (lần %d/6, %s); thử lại sau 5 giây...", attempt, result.Verification.Outcome)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return false, nil
}

// startTunnel runs cloudflared and returns its public trycloudflare.com URL.
func startTunnel(ctx context.Context, port string) (string, error) {
	cmd := exec.CommandContext(ctx, "cloudflared", "tunnel", "--no-autoupdate", "--url", "http://127.0.0.1:"+port)
	output, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("không chạy được cloudflared (đã cài chưa?): %w", err)
	}
	found := make(chan string, 1)
	go func() {
		// Keep draining output, otherwise cloudflared blocks once the pipe buffer is full.
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			for _, u := range tunnelFinder.FindAllString(scanner.Text(), -1) {
				if u != "https://api.trycloudflare.com" {
					select {
					case found <- u:
					default:
					}
				}
			}
		}
		_ = cmd.Wait()
		if ctx.Err() == nil {
			log.Print("cloudflared đã dừng; Zalo không gửi được tin tới bot nữa. Hãy chạy lại.")
		}
	}()
	select {
	case u := <-found:
		return u, nil
	case <-time.After(45 * time.Second):
		return "", errors.New("cloudflared không trả URL sau 45 giây")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
