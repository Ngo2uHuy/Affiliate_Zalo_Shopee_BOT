# Subagent Dispatch & Model Selection Rules

Tài liệu quy tắc dành cho hệ thống agent và subagent trong workspace, đảm bảo tối ưu chi phí và độ chính xác theo mô hình Gemini.

---

## 1. Dispatch Subagent — Tự chọn Model + Effort Rẻ Nhất

Mọi lần gọi subagent, background task, hoặc phân rã tác vụ (bao gồm các bước trong Skills/Hooks) **phải chỉ định mức theo bảng dưới, KHÔNG ĐƯỢC ĐỂ MẶC ĐỊNH**. Nếu không chỉ định, subagent sẽ kế thừa model phiên chính (thường là Pro/High Effort), dẫn đến việc đọc file hoặc chạy lệnh build cũng tính giá đắt nhất.

## 2. Bảng Phân Cấp Model & Effort (Gemini Ecosystem)

| Mức | Model & Cấu hình | Vai trò | Dùng cho |
|---|---|---|---|
| **T1: Flash** | **Gemini Flash / Flash-Lite**<br>*(Effort: Low / None)* | `Explorer` (Chỉ đọc hoặc sửa cơ học) | Tìm file/symbol/chỗ dùng; đọc rồi tóm tắt; chạy build/test rồi lọc lỗi; sửa cơ học đã chỉ rõ từng chỗ (đổi tên biến, thêm key config/i18n theo mẫu, sửa typo). |
| **T2: Balanced** | **Gemini Pro (Effort: Medium)**<br>hoặc **Gemini Flash (Effort: High)** | `Implementer` | Implement theo plan/spec đã chốt; viết test; sửa lỗi build/test đã rõ nguyên nhân; soát spec compliance. |
| **T3: Deep Worker** | **Gemini Pro**<br>*(Effort: High / Deep Thinking)* | `Deep-Worker` | Debug chưa rõ nguyên nhân; lập plan; review chất lượng/bảo mật; migration DB, auth/multi-tenant, thay đổi chạm nhiều module. |
| **Thiết kế** | **Gemini Pro**<br>*(Effort: Max / Full Thinking)* | `Architect` | Thiết kế kiến trúc, mô hình dữ liệu, luồng nghiệp vụ; đề xuất và so sánh phương án. |
| **T4: Extreme** | **Gemini Ultra / Exp**<br>*(Effort: Max / Unrestricted)* | `Specialized` | **KHÔNG TỰ Ý DÙNG** — Chỉ khi tôi yêu cầu. |

---

## 3. Nguyên Tắc Vận Hành
1. **Phạm vi giới hạn (Bounded Scope)**: Subagent chỉ nhận context cần thiết và trả về kết quả theo format định sẵn.
2. **Fail-fast**: Nếu T1 không giải quyết được sau 1-2 turn, phải bàn giao cho T2/T3, không lặp lại tốn token.
3. **Escalate tự động**: Khi phát hiện rủi ro bảo mật hoặc sửa đổi schema, tự động nâng tier lên T3.
