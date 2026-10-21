# Affiliate BOT — Zalo Bot đổi link Shopee Affiliate

## Luồng AffiPad tương tự link GoDeal

Bot yêu cầu API rút gọn bằng `useShortLink=true` và ưu tiên trường `shortUrl` hợp lệ. Link rút gọn nhà cung cấp được gửi trực tiếp trong Zalo, không bọc thêm Quick Tunnel; nếu không có, bot dùng link kết quả hợp lệ và rút gọn nội bộ khi cần.

Trong `.env`, đặt `SHOPEE_MODE=affipad`, điền `AFFIPAD_API_KEY` và `AFFIPAD_TOOL_ID` từ workspace AffiPad (danh sách tool có tại `GET https://api.affipad.com/v1/tools`). Không cần Shopee App ID/Secret trong chế độ này. Chạy `go run . -tunnel`, gửi sản phẩm cho bot để nhận link `/r/<id>` → `s.afp.ad` → Shopee. Nếu không chạy tunnel, cấu hình `EXPERIMENTAL_LINK_BASE_URL` cho tên miền của bot; nếu không có tên miền, bot trả trực tiếp link AffiPad.

Tích hợp dùng `POST /v1/convert`, preset `fb_reels`, giữ link redirect của nhà cung cấp, không tự thêm tham số Facebook giả hoặc sửa payload/chữ ký. Trên điện thoại, link AffiPad được redirect nguyên trạng để nhà cung cấp xử lý mở app. Bot ưu tiên link `s.afp.ad` và cũng chấp nhận link HTTPS Shopee hợp lệ do AffiPad trả về (ví dụ `s.shopee.vn/an_redir`); tên miền đầu ra không chứng minh khả năng áp mã. API có quota theo tài khoản AffiPad. Không tái sử dụng token hay affiliate ID từ link GoDeal mẫu.

Chưa xác minh áp mã trên tài khoản Shopee thật. API convert không trả danh sách voucher đủ điều kiện theo sản phẩm/tài khoản, nên bot không tự khẳng định mức giảm hoặc tỷ lệ áp mã. Link nội bộ vẫn lưu RAM, hết hạn sau 24 giờ hoặc restart. Tài liệu API: https://docs.affipad.com/api

## Thử nghiệm link rút gọn nguồn Facebook

Trên điện thoại, link `/r/<id>` tự thử mở app một lần khi trang hiển thị; nếu bị chặn vẫn có nút **Mở ứng dụng Shopee** và **Tiếp tục trên web**. Trang dùng deep link `shopeevn://reactPath?navigate_url=<link Shopee>&path=shopee/TRANSFER_PAGE...` (cùng định dạng trang universal-link của Shopee), vì trình duyệt trong Zalo bỏ qua Universal Link/App Link HTTPS; Android gọi qua Intent tới package `com.shopee.vn` kèm URL web dự phòng, iPhone/iPad gọi thẳng scheme `shopeevn://`. Giữ nguyên link affiliate và tham số thử nghiệm. Khả năng mở app phụ thuộc Shopee, thiết bị và trình duyệt Zalo; nếu bị chặn, mở trang trong trình duyệt ngoài rồi bấm lại. Desktop vẫn redirect HTTP 302. Chưa kiểm chứng việc mở app trên thiết bị thật.

Thêm `EXPERIMENTAL_LINK_BASE_URL=https://ten-mien-bot-cua-ban` vào `.env` rồi khởi động lại. Tên miền phải trỏ tới server bot hiện tại; để trống để tắt. Bot Zalo (cả demo và live) sẽ trả link `/r/<id>` do hệ thống tạo. Khi mở, server redirect tới link Shopee, giữ tham số có sẵn và thêm `utm_source=facebook`, `utm_medium=social`, `fbclid=experimental_<id>`.

Đây là tham số giả lập để thử nghiệm, không phải click ID Facebook thật và chưa chứng minh khả năng áp voucher. Không giả được Referer của trình duyệt bằng response redirect. Kiểm tra mã tại checkout Shopee; demo không tạo hoa hồng. Link lưu trong RAM, hết hạn sau 24 giờ hoặc khi bot khởi động lại, tối đa 10.000 link còn hạn. Nếu kho đầy, bot trả link Shopee gốc. Khi chạy với `-tunnel`, bot tự bật link thử nghiệm và dùng địa chỉ Quick Tunnel hiện tại, ưu tiên hơn cấu hình tên miền; không cần điền EXPERIMENTAL_LINK_BASE_URL. API web vẫn giữ hành vi cũ.

Khách gửi link sản phẩm Shopee (qua Zalo hoặc trang web), bot đổi sang link tiếp thị liên kết bằng Shopee Affiliate Open API rồi trả lời kèm:

- link mua hàng (`s.shopee.vn/...`) tính hoa hồng cho bạn;
- link bài Facebook "săn mã" để khách lấy mã độc quyền Facebook/Instagram (tuỳ chọn);
- link bài review Facebook của sản phẩm (tuỳ chọn);
- danh sách mã giảm giá công khai của Shopee (khi khách gọi lệnh).

Viết bằng Go, chỉ dùng thư viện chuẩn, không cần cơ sở dữ liệu.

## Yêu cầu

- Go 1.22 trở lên.
- [cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/): chỉ cần khi chạy với `-tunnel`.
- Bot Zalo (lấy `ZALO_BOT_TOKEN` từ Zalo Bot Platform).
- Tài khoản Shopee Affiliate có quyền Open API (App ID + Secret): chỉ cần cho chế độ `live`.

## Chạy nhanh (Windows)

1. Nhấp đúp `start.cmd`. Lần đầu, file sẽ tạo `.env` từ `.env.example` và mở Notepad.
2. Điền `ZALO_BOT_TOKEN` và `ZALO_WEBHOOK_SECRET`, lưu lại rồi chạy lại `start.cmd`.
3. Bot tự mở Cloudflare Quick Tunnel, tự đăng ký webhook với Zalo và in ra địa chỉ trang web công khai.

Chạy bằng dòng lệnh:

```sh
go run . -tunnel                                     # tự mở tunnel và đăng ký webhook
go run .                                             # chỉ chạy server ở 127.0.0.1:PORT
go run . set-webhook https://ten-mien-cua-ban        # đăng ký webhook với tên miền riêng
go run . -env duong-dan/.env                         # dùng file cấu hình khác
go test ./...                                        # chạy test
```

Server chỉ nghe ở `127.0.0.1`, nên traffic bên ngoài phải đi qua tunnel hoặc reverse proxy. Địa chỉ Quick Tunnel đổi sau mỗi lần chạy, và bot tự đăng ký lại webhook.

## Cấu hình `.env`

| Biến | Bắt buộc | Ý nghĩa |
|---|---|---|
| `ZALO_BOT_TOKEN` | Có | Token bot Zalo, dạng `123456:abc...` |
| `ZALO_WEBHOOK_SECRET` | Có | Chuỗi ngẫu nhiên 8–256 ký tự; Zalo gửi kèm mỗi request để bot xác thực |
| `SHOPEE_MODE` | Không | `demo` (mặc định, chỉ nhắc lại link sạch, không tạo hoa hồng) hoặc `live` |
| `SHOPEE_APP_ID`, `SHOPEE_SECRET` | Khi `live` | Thông tin Shopee Affiliate Open API |
| `FACEBOOK_PAGE_URL` | Không | Trang Facebook gửi kèm khi sản phẩm chưa có bài review riêng |
| `FACEBOOK_DEAL_POST_URL` | Không | Bài Facebook "săn mã", nơi đăng link affiliate trong bình luận |
| `FACEBOOK_DEAL_POST_ID` | Không | ID bài, dạng `pageid_postid`; điền cùng `FACEBOOK_PAGE_TOKEN` để bot tự comment |
| `FACEBOOK_PAGE_TOKEN` | Không | Page access token có quyền `pages_manage_engagement` |
| `PORT` | Không | Cổng server, mặc định `8080` |
| `WORKERS` | Không | Số luồng xử lý tin nhắn (1–32), mặc định `4` |

Không chia sẻ hoặc đưa `.env` lên Git (đã có trong `.gitignore`).

## Cách dùng

### Trên Zalo

- **Chat riêng:** gửi link Shopee để nhận link mua hàng. Gửi tin khác thì bot trả lời hướng dẫn.
- **Nhóm:** bot chỉ trả lời khi tin nhắn có link Shopee hoặc có lệnh mã giảm giá, còn lại bot im lặng.
- **Lệnh mã giảm giá:** `mã giảm giá`, `voucher` hoặc `/magiamgia`; thêm số trang để xem tiếp, ví dụ `mã giảm giá 2`.

Bot nhận link đầy đủ (`shopee.vn/...-i.<shop>.<item>`, `shopee.vn/product/<shop>/<item>`) và link rút gọn (`s.shopee.vn`, `vn.shp.ee`, `shope.ee`...). Link được làm sạch về `https://shopee.vn/product/<shop>/<item>` trước khi đổi, nên tham số theo dõi của affiliate khác không bị giữ lại.

### Trang web

Mở địa chỉ tunnel (hoặc `http://127.0.0.1:8080/`), dán link để nhận link mua hàng và xem danh sách mã. Mỗi IP được đổi tối đa 10 link mỗi phút.

### Theo dõi nguồn đơn (subId)

| subId | Nguồn |
|---|---|
| `zalo_bot` | Link tạo qua bot Zalo |
| `web` | Link tạo trên trang web |
| `web_facebook` | Link tạo trên trang web, khi khách vào trang từ Facebook (có `fbclid` hoặc Referer Facebook) |

Xem thống kê theo subId trong báo cáo Shopee Affiliate.

## Mã độc quyền Facebook/Instagram

Shopee chỉ áp mã độc quyền Facebook/Instagram khi khách **thật sự** mở link từ Facebook. Bot không giả mạo nguồn truy cập; thay vào đó, bot dẫn khách sang bài Facebook "săn mã":

1. Tạo một bài trên **Fanpage** và điền link bài vào `FACEBOOK_DEAL_POST_URL`.
2. (Tuỳ chọn) Điền `FACEBOOK_DEAL_POST_ID` và `FACEBOOK_PAGE_TOKEN` để bot tự comment link affiliate lên bài sau mỗi lần đổi link. Khi đó tin trả lời ghi *"Link mã Facebook/Instagram (cmt mới nhất)"*.
   - Facebook không cho API comment lên trang cá nhân. Nếu bài nằm trên trang cá nhân, hãy bỏ trống hai biến này và tự comment link.
   - Nếu comment lỗi, bot vẫn gửi link bài và ghi lỗi vào log.
3. Tạo file `deal_vouchers.txt` cạnh file chạy, ghi danh sách mã sẽ gửi kèm, ví dụ:

   ```text
   🔴 Mã Youtube:
   - Giảm 22% tối đa 100k đơn từ 150k
   - BACK MÃ LÚC: 0H · 9H · 12H · 18H MỖI NGÀY

   🟣 Mã Instagram:
   - Giảm 22% tối đa 300k đơn từ 50k
   ```

   Bot đọc lại file mỗi lần trả lời, nên sửa lúc bot đang chạy cũng được (tối đa 4000 ký tự).

## Các file dữ liệu

| File | Nội dung | Nạp lại khi đang chạy |
|---|---|---|
| `shopee_sources.json` | Danh sách mã Shopee (`promotionid`, `voucher_code`, `signature`) dùng cho lệnh mã giảm giá | Không, cần khởi động lại |
| `facebook_posts.json` | Nối sản phẩm với bài review Facebook: `[{"product": "<link Shopee>", "post": "<link Facebook>"}]` | Có, theo thời gian sửa file |
| `deal_vouchers.txt` | Nội dung mã gửi kèm link bài săn mã | Có, đọc mỗi lần trả lời |

Mã giảm giá được lấy từ trang công khai của Shopee mỗi 15 phút (thử lại sau 1 phút nếu lỗi). Đây không phải API đối tác chính thức, nên không bảo đảm mã áp dụng được cho mọi sản phẩm hoặc tài khoản.

## Cấu trúc mã nguồn

| File | Vai trò |
|---|---|
| `main.go` | Cấu hình, webhook Zalo, hàng đợi xử lý, gọi Shopee Open API, gửi tin Zalo, tunnel |
| `web.go`, `web/index.html` | Trang web công khai và API `/api/convert`, `/api/vouchers` |
| `vouchers.go` | Lấy và phân trang danh sách mã giảm giá |
| `backlinks.go` | Link bài review Facebook theo sản phẩm |
| `deals.go` | Bài Facebook săn mã: tự comment link và ghép nội dung mã |

Endpoint: `POST /webhook` (Zalo), `GET /` (trang web), `POST /api/convert`, `GET /api/vouchers`, `GET /healthz`.

## Lưu ý khi chạy thật

- Hàng đợi tin nhắn và bộ chống trùng nằm trong RAM. Tin vừa nhận ngay trước khi tắt hoặc khởi động lại bot có thể bị mất. Khi hàng đợi đầy (1024 tin), bot trả HTTP 503 để Zalo gửi lại.
- Quick Tunnel đổi địa chỉ mỗi lần chạy. Nếu muốn địa chỉ cố định, dùng Cloudflare Tunnel có tên miền riêng rồi chạy `go run . set-webhook https://ten-mien`.
- Link tạo ra là link tiếp thị liên kết; tin trả lời có ghi rõ điều này cho khách.
