package api

import (
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

const (
	adminCookieName = "admin_token"
	adminSessionTTL = 24 * time.Hour

	adminLoginFailureLimit = 5
	adminLoginWindow       = 15 * time.Minute
	adminLoginLockDuration = 15 * time.Minute
	maxAdminLoginTrackers  = 10_000
)

type adminLoginAttempt struct {
	Failures      int
	WindowStarted time.Time
	LockedUntil   time.Time
	LastSeen      time.Time
}

var (
	//nolint:gochecknoglobals // Admin sessions state
	adminSessionsMu sync.Mutex
	//nolint:gochecknoglobals // Admin sessions state
	adminSessions = map[string]time.Time{}
	//nolint:gochecknoglobals // Bounded in-memory login abuse protection.
	adminLoginAttemptsMu sync.Mutex
	//nolint:gochecknoglobals // Bounded in-memory login abuse protection.
	adminLoginAttempts = map[string]adminLoginAttempt{}
)

func adminClientIP(r *http.Request) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}

	trustForwarded := strings.TrimSpace(os.Getenv("RENDER")) != "" ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("VPROXY_TRUST_PROXY_HEADERS")), "true")
	if !trustForwarded {
		return remote
	}
	// 取 X-Forwarded-For 最右侧条目：该段由我们信任的前置代理写入，客户端无法伪造。
	// 最左侧是客户端自称的地址，攻击者可为每次请求编造不同值，从而绕过按 IP 的登录限流。
	forwarded := r.Header.Get("X-Forwarded-For")
	parts := strings.Split(forwarded, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		if candidate == "" {
			continue
		}
		if parsed := net.ParseIP(strings.Trim(candidate, "[]")); parsed != nil {
			return parsed.String()
		}
	}
	return remote
}

func adminLoginRetryAfter(clientIP string, now time.Time) int {
	adminLoginAttemptsMu.Lock()
	defer adminLoginAttemptsMu.Unlock()
	attempt, ok := adminLoginAttempts[clientIP]
	if !ok || !now.Before(attempt.LockedUntil) {
		return 0
	}
	return max(1, int((attempt.LockedUntil.Sub(now)+time.Second-1)/time.Second))
}

func recordAdminLoginFailure(clientIP string, now time.Time) int {
	adminLoginAttemptsMu.Lock()
	defer adminLoginAttemptsMu.Unlock()

	if len(adminLoginAttempts) >= maxAdminLoginTrackers {
		staleBefore := now.Add(-time.Hour)
		for key, item := range adminLoginAttempts {
			if item.LastSeen.Before(staleBefore) && !now.Before(item.LockedUntil) {
				delete(adminLoginAttempts, key)
			}
		}
		for len(adminLoginAttempts) >= maxAdminLoginTrackers {
			for key := range adminLoginAttempts {
				delete(adminLoginAttempts, key)
				break
			}
		}
	}

	attempt := adminLoginAttempts[clientIP]
	if attempt.WindowStarted.IsZero() || now.Sub(attempt.WindowStarted) >= adminLoginWindow {
		attempt.Failures = 0
		attempt.WindowStarted = now
		attempt.LockedUntil = time.Time{}
	}
	attempt.Failures++
	attempt.LastSeen = now
	if attempt.Failures >= adminLoginFailureLimit {
		attempt.LockedUntil = now.Add(adminLoginLockDuration)
	}
	adminLoginAttempts[clientIP] = attempt
	if now.Before(attempt.LockedUntil) {
		return max(1, int((attempt.LockedUntil.Sub(now)+time.Second-1)/time.Second))
	}
	return 0
}

func clearAdminLoginFailures(clientIP string) {
	adminLoginAttemptsMu.Lock()
	delete(adminLoginAttempts, clientIP)
	adminLoginAttemptsMu.Unlock()
}

func cleanupAdminLoginAttempts(now time.Time) {
	staleBefore := now.Add(-time.Hour)
	adminLoginAttemptsMu.Lock()
	for key, item := range adminLoginAttempts {
		if item.LastSeen.Before(staleBefore) && !now.Before(item.LockedUntil) {
			delete(adminLoginAttempts, key)
		}
	}
	adminLoginAttemptsMu.Unlock()
}

func issueAdminToken() string {
	b := make([]byte, 32)
	_, _ = cryptorand.Read(b)
	tok := hex.EncodeToString(b)
	adminSessionsMu.Lock()
	adminSessions[tok] = time.Now().Add(adminSessionTTL)
	adminSessionsMu.Unlock()
	return tok
}

func checkAdminToken(tok string) bool {
	if tok == "" {
		return false
	}
	adminSessionsMu.Lock()
	defer adminSessionsMu.Unlock()
	exp, ok := adminSessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(adminSessions, tok)
		return false
	}
	return true
}

func dropAdminToken(tok string) {
	if tok == "" {
		return
	}
	adminSessionsMu.Lock()
	delete(adminSessions, tok)
	adminSessionsMu.Unlock()
}

func cleanupAdminSessions() int {
	now := time.Now()
	adminSessionsMu.Lock()
	defer adminSessionsMu.Unlock()
	n := 0
	for tok, exp := range adminSessions {
		if now.After(exp) {
			delete(adminSessions, tok)
			n++
		}
	}
	return n
}

func adminTokenFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if c, err := r.Cookie(adminCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

func requireAdmin(r *http.Request) bool {
	return checkAdminToken(adminTokenFromRequest(r))
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	forwardedProto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(forwardedProto, "https")
}

func adminRequestOriginAllowed(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(
		strings.ToLower(auth),
		"bearer ",
	) {
		return true
	}
	if _, err := r.Cookie(adminCookieName); err != nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// Non-browser clients and older same-origin browsers may omit Origin.
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	return strings.EqualFold(parsed.Scheme, scheme) && strings.EqualFold(parsed.Host, r.Host)
}

func StartAdminSessionCleanup(interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if n := cleanupAdminSessions(); n > 0 {
				log.Printf("[Admin] 已清理 %d 个过期会话 token", n)
			}
			cleanupAdminLoginAttempts(time.Now())
		}
	}()
}

func EnsureAdminPassword() {
	if strings.TrimSpace(config.Load().AdminPassword) != "" {
		return
	}
	b := make([]byte, 9)
	if _, err := cryptorand.Read(b); err != nil {
		log.Printf("[Admin] 生成管理员密码失败：%v", err)
		return
	}
	pw := base64.RawURLEncoding.EncodeToString(b)
	if err := config.WriteSettings(map[string]any{"admin_password": pw}); err != nil {
		log.Printf("[Admin] 写入管理员密码到 config.json 失败：%v", err)
		return
	}
	bar := strings.Repeat("=", 60)
	log.Printf("%s", bar)
	log.Printf("[Admin] 首次启动，已自动生成管理员密码：")
	log.Printf("[Admin]     密码: %s", pw)
	log.Printf("[Admin]     访问: http://<host>:<port>/admin")
	log.Printf("[Admin]     密码已写入 config/config.json，登录后可在面板修改")
	log.Printf("%s", bar)
}

func (adm *AdminHandler) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}
	clientIP := adminClientIP(r)
	if retryAfter := adminLoginRetryAfter(clientIP, time.Now()); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, adminErr("登录尝试过多，请稍后再试"))
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	expected := strings.TrimSpace(config.Load().AdminPassword)
	if expected == "" {
		writeJSON(w, http.StatusInternalServerError, adminErr("管理员密码未初始化 (admin password not set)"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Password)), []byte(expected)) != 1 {
		retryAfter := recordAdminLoginFailure(clientIP, time.Now())
		log.Printf("[Security] 警告：后台登录失败，密码错误。来源 IP: %s", clientIP)
		if retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeJSON(w, http.StatusTooManyRequests, adminErr("登录尝试过多，请稍后再试"))
			return
		}
		writeJSON(w, http.StatusUnauthorized, adminErr("密码错误 (invalid password)"))
		return
	}
	clearAdminLoginFailures(clientIP)
	log.Printf("[Admin] 管理后台登录成功。来源 IP: %s", clientIP)
	cleanupAdminSessions()
	tok := issueAdminToken()
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   int(adminSessionTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminLogout(w http.ResponseWriter, r *http.Request) {
	dropAdminToken(adminTokenFromRequest(r))
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminCheckAuth(w http.ResponseWriter, r *http.Request) {
	authenticated := requireAdmin(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":     authenticated,
		"background_image":  adm.cfg.BackgroundImage(),
		"font_size":         adm.cfg.FontSize(),
		"font_color_type":   adm.cfg.FontColorType(),
		"font_color":        adm.cfg.FontColor(),
		"custom_bg_presets": adm.cfg.CustomBgPresets(),
	})
}

func (adm *AdminHandler) adminChangePassword(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(os.Getenv("VPROXY_ADMIN_PASSWORD")) != "" {
		writeJSON(
			w,
			http.StatusConflict,
			adminErr("管理员密码由环境变量托管，请在 Render 环境设置中修改"),
		)
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	expected := strings.TrimSpace(config.Load().AdminPassword)
	if expected == "" {
		writeJSON(w, http.StatusInternalServerError, adminErr("未设置管理员密码"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.OldPassword)), []byte(expected)) != 1 {
		writeJSON(w, http.StatusBadRequest, adminErr("原密码错误"))
		return
	}
	newPw := strings.TrimSpace(body.NewPassword)
	if len(newPw) < 6 {
		writeJSON(w, http.StatusBadRequest, adminErr("新密码不能少于 6 个字符"))
		return
	}
	if err := config.WriteSettings(map[string]any{"admin_password": newPw}); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("写入新密码失败"))
		return
	}
	adminSessionsMu.Lock()
	adminSessions = map[string]time.Time{}
	adminSessionsMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
	log.Printf("[Security] 后台管理员密码已修改，所有在线会话已重置。来源 IP: %s", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
