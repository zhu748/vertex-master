package api

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/admin"
	"github.com/bsfdsagfadg/vertex/internal/config"
)

const (
	maxBackgroundUploadBytes  = 10 << 20
	maxBackgroundUploadPixels = 40_000_000
	maxAdminLogTailBytes      = 1 << 20
	maxAdminLogTailLines      = 200
	adminLogReadBlockSize     = 32 * 1024
	defaultAdminBackground    = "url('background.jpg')"
)

var adminLogReadBlockPool = sync.Pool{ //nolint:gochecknoglobals
	New: func() any { return new([adminLogReadBlockSize]byte) },
}

type AdminHandler struct {
	handler
	backgroundUploadMu sync.Mutex
	claudePrompts      *claudePromptStore
}

func (adm *AdminHandler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/admin")
	log.Printf("[Server] [AdminAPI] 收到请求: %s %s", r.Method, path)
	requireMethod := func(methods ...string) bool {
		for _, method := range methods {
			if r.Method == method {
				return true
			}
		}
		adm.adminMethodNotAllowed(w)
		return false
	}

	if path == "/login" {
		adm.adminLogin(w, r)
		return
	}
	if path == "/check-auth" {
		if !requireMethod(http.MethodGet) {
			return
		}
		adm.adminCheckAuth(w, r)
		return
	}

	if strings.HasPrefix(path, "/keys/") {
		if !requireAdmin(r) {
			adm.adminUnauthorized(w)
			return
		}
		if !adminRequestOriginAllowed(r) {
			writeJSON(w, http.StatusForbidden, adminErr("管理请求来源校验失败"))
			return
		}
		if !requireMethod(http.MethodDelete) {
			return
		}
		adm.adminDeleteKey(w, r, strings.TrimPrefix(path, "/keys/"))
		return
	}

	if !requireAdmin(r) {
		adm.adminUnauthorized(w)
		return
	}
	if !adminRequestOriginAllowed(r) {
		writeJSON(w, http.StatusForbidden, adminErr("管理请求来源校验失败"))
		return
	}

	switch path {
	case "/nodes":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetNodes(w, r)
		case http.MethodPost:
			adm.adminAddStandardProxy(w, r)
		case http.MethodDelete:
			adm.adminDeleteNode(w, r)
		default:
			adm.adminMethodNotAllowed(w)
		}
		return
	case "/proxy-subscriptions":
		switch r.Method {
		case http.MethodGet:
			adm.adminListProxySubscriptions(w, r)
		case http.MethodPost, http.MethodPut:
			adm.adminSaveProxySubscription(w, r)
		case http.MethodDelete:
			adm.adminDeleteProxySubscription(w, r)
		default:
			adm.adminMethodNotAllowed(w)
		}
		return
	case "/proxy-subscriptions/refresh":
		if r.Method != http.MethodPost {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminRefreshProxySubscription(w, r)
		return
	case "/nodes/test":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminTestNode(w, r)
		return
	case "/nodes/current":
		if !requireMethod(http.MethodGet) {
			return
		}
		adm.adminGetRecentProxy(w, r)
		return
	case "/nodes/enable":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminEnableNode(w, r)
		return
	case "/nodes/test-all":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminTestAll(w, r)
		return
	case "/nodes/test-progress":
		if !requireMethod(http.MethodGet) {
			return
		}
		adm.adminGetTestProgress(w, r)
		return
	case "/nodes/test-pause":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminTestPause(w, r)
		return
	case "/nodes/test-resume":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminTestResume(w, r)
		return
	case "/nodes/test-terminate":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminTestTerminate(w, r)
		return
	case "/nodes/deduplicate":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminDedupNodes(w, r)
		return
	case "/nodes/disabled":
		if !requireMethod(http.MethodDelete) {
			return
		}
		adm.adminDeleteDisabledNodes(w, r)
		return
	case "/nodes/import":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminImportNodes(w, r)
		return
	case "/nodes/import-json":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminImportNodesJson(w, r)
		return
	case "/subscriptions/fetch":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminFetchSub(w, r)
		return
	case "/use-node":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminUseNode(w, r)
		return
	case "/nodes/batch-disable":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminBatchDisableNodes(w, r)
		return
	case "/nodes/batch-enable":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminBatchEnableNodes(w, r)
		return
	case "/nodes/batch-delete":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminBatchDeleteNodes(w, r)
		return
	case "/nodes/sort":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminSortNodesByLatency(w, r)
		return
	case "/upload-bg":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminUploadBg(w, r)
		return
	case "/delete-bg":
		if !requireMethod(http.MethodDelete, http.MethodPost) {
			return
		}
		adm.adminDeleteBg(w, r)
		return
	case "/list-bgs":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminListBgs(w, r)
		return
	}

	switch path {
	case "/logout":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminLogout(w, r)
	case "/password":
		if r.Method == http.MethodPost {
			adm.adminChangePassword(w, r)
		} else {
			adm.adminMethodNotAllowed(w)
		}
	case "/settings":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetSettings(w, r)
		case http.MethodPut:
			adm.adminPutSettings(w, r)
		default:
			adm.adminMethodNotAllowed(w)
		}
	case "/claude-prompt/latest":
		adm.adminClaudePromptLatest(w, r)
	case "/stats":
		if !requireMethod(http.MethodGet) {
			return
		}
		adm.adminGetStats(w, r)
	case "/stats/reset":
		if !requireMethod(http.MethodPost) {
			return
		}
		adm.adminResetStats(w, r)
	case "/log":
		if r.Method != http.MethodGet {
			adm.adminMethodNotAllowed(w)
			return
		}
		adm.adminGetLog(w, r)
	case "/keys":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetKeys(w, r)
		case http.MethodPost:
			adm.adminAddKey(w, r)
		default:
			adm.adminMethodNotAllowed(w)
		}
	case "/models":
		switch r.Method {
		case http.MethodGet:
			adm.adminGetModels(w, r)
		case http.MethodPut:
			adm.adminPutModels(w, r)
		default:
			adm.adminMethodNotAllowed(w)
		}
	default:
		writeJSON(w, http.StatusNotFound, adminErr("未知接口 (not found)"))
	}
}

func (adm *AdminHandler) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" {
		name = "admin.html"
	}
	data, err := fs.ReadFile(admin.Assets, "assets/"+name)
	if err != nil {
		oaiError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".gif"):
		return "image/gif"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func (adm *AdminHandler) adminUploadBg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}

	err := r.ParseMultipartForm(1 << 20)
	if err != nil {
		if isRequestBodyTooLarge(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, adminErr("请求体过大 (request body too large)"))
			return
		}
		writeJSON(w, http.StatusBadRequest, adminErr("解析上传文件失败 (parse error)"))
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("未找到文件字段 (missing file)"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBackgroundUploadBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr("读取上传文件失败"))
		return
	}
	if len(data) == 0 || len(data) > maxBackgroundUploadBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, adminErr("背景图片不能超过 10 MiB"))
		return
	}
	extension, err := validateBackgroundImage(data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, adminErr(err.Error()))
		return
	}

	adm.backgroundUploadMu.Lock()
	defer adm.backgroundUploadMu.Unlock()

	assetsDir := filepath.Join(filepath.Dir(adm.cfg.ConfigDir()), "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("无法创建资源目录 (create directory error)"))
		return
	}

	filename := fmt.Sprintf("background%d%s", time.Now().UnixNano(), extension)
	targetPath := filepath.Join(assetsDir, filename)

	if err := writeBackgroundFileAtomically(targetPath, data); err != nil {
		writeJSON(w, http.StatusInternalServerError, adminErr("无法保存文件 (create error)"))
		return
	}

	bgURL := "url('/assets/" + filename + "')"
	err = config.WriteSettings(map[string]any{"background_image": bgURL})
	if err != nil {
		_ = os.Remove(targetPath)
		writeJSON(w, http.StatusInternalServerError, adminErr("更新配置失败 (save config error)"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": bgURL})
}

func writeBackgroundFileAtomically(targetPath string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(targetPath), ".background-upload-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o644); err != nil {
		return err
	}
	written, err := temp.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		return err
	}
	committed = true
	return nil
}

func backgroundAssetFilename(name string) bool {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	lower := strings.ToLower(name)
	if !strings.HasPrefix(lower, "background") {
		return false
	}
	switch filepath.Ext(lower) {
	case ".jpg", ".jpeg", ".png", ".gif":
		return true
	default:
		return false
	}
}

func validateBackgroundImage(data []byte) (string, error) {
	imageConfig, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("仅支持有效的 JPEG、PNG 或 GIF 图片")
	}
	if imageConfig.Width < 1 || imageConfig.Height < 1 ||
		int64(imageConfig.Width)*int64(imageConfig.Height) > maxBackgroundUploadPixels {
		return "", fmt.Errorf("背景图片像素尺寸过大，最多 4000 万像素")
	}
	switch format {
	case "jpeg":
		return ".jpg", nil
	case "png":
		return ".png", nil
	case "gif":
		return ".gif", nil
	default:
		return "", fmt.Errorf("不支持的背景图片格式")
	}
}

func (adm *AdminHandler) adminDeleteBg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		adm.adminMethodNotAllowed(w)
		return
	}
	var body struct {
		Filename string `json:"filename"`
	}
	if !adm.decodeAdminBody(w, r, &body) {
		return
	}
	if body.Filename == "" || strings.Contains(body.Filename, "/") || strings.Contains(body.Filename, "\\") {
		writeJSON(w, http.StatusBadRequest, adminErr("文件名无效"))
		return
	}
	if !backgroundAssetFilename(body.Filename) {
		writeJSON(w, http.StatusForbidden, adminErr("无权删除该文件"))
		return
	}

	adm.backgroundUploadMu.Lock()
	defer adm.backgroundUploadMu.Unlock()

	assetsDir := filepath.Join(filepath.Dir(adm.cfg.ConfigDir()), "assets")
	targetPath := filepath.Join(assetsDir, body.Filename)
	assetURL := "url('/assets/" + body.Filename + "')"
	updates := make(map[string]any, 2)
	if adm.cfg.BackgroundImage() == assetURL {
		updates["background_image"] = defaultAdminBackground
	}
	presets := adm.cfg.CustomBgPresets()
	filtered := make([]string, 0, len(presets))
	for _, preset := range presets {
		if preset != assetURL {
			filtered = append(filtered, preset)
		}
	}
	if len(filtered) != len(presets) {
		updates["custom_bg_presets"] = filtered
	}
	if len(updates) > 0 {
		if err := config.WriteSettings(updates); err != nil {
			writeJSON(w, http.StatusInternalServerError, adminErr("更新背景配置失败"))
			return
		}
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, adminErr("删除文件失败"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (adm *AdminHandler) adminListBgs(w http.ResponseWriter, r *http.Request) {
	assetsDir := filepath.Join(filepath.Dir(adm.cfg.ConfigDir()), "assets")
	files, err := os.ReadDir(assetsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": []string{}})
		return
	}

	var bgs []string
	for _, f := range files {
		if !f.IsDir() && backgroundAssetFilename(f.Name()) {
			bgs = append(bgs, f.Name())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": bgs})
}

func (adm *AdminHandler) adminGetLog(w http.ResponseWriter, r *http.Request) {
	logPath := filepath.Join(filepath.Dir(adm.cfg.ConfigDir()), "logs", "logs_latest.log")

	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		logPath = "logs/logs_latest.log"
	}

	content, err := readAdminLogTail(logPath, maxAdminLogTailBytes, maxAdminLogTailLines)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "content": ""})
			return
		}
		writeJSON(w, http.StatusInternalServerError, adminErr("无法读取日志文件 (read error)"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "content": content})
}

// readAdminLogTail reads a bounded snapshot from the end of a potentially
// large, actively-written log and returns at most maxLines non-empty lines.
func readAdminLogTail(path string, maxBytes int64, maxLines int) (string, error) {
	if maxBytes <= 0 || maxLines <= 0 {
		return "", nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() <= 0 {
		return "", nil
	}

	lines := make([][]byte, 0, min(maxLines, 64))
	contentBytes := 0
	windowStart := max(int64(0), info.Size()-maxBytes)
	position := info.Size()
	var pending []byte
	sawNewline := false
	readBlock := adminLogReadBlockPool.Get().(*[adminLogReadBlockSize]byte)
	defer adminLogReadBlockPool.Put(readBlock)
	usePooledBlock := true
	for position > windowStart && len(lines) < maxLines {
		start := max(windowStart, position-int64(adminLogReadBlockSize))
		var block []byte
		if usePooledBlock {
			block = readBlock[:int(position-start)]
			usePooledBlock = false
		} else {
			block = make([]byte, int(position-start))
		}
		n, readErr := file.ReadAt(block, start)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		block = block[:n]
		if len(block) == 0 {
			break
		}
		// 文件末尾若连续 32KiB 都没有换行，继续逐块前插会让单个超长行
		// 产生 O(n²) 复制。改用预扩容 Builder 读取整个有界窗口；纯单行
		// 可以直接返回 Builder 的字符串，不再额外复制一份同样大的结果。
		if position == info.Size() && start > windowStart && bytes.IndexByte(block, '\n') < 0 {
			window, windowErr := readAdminLogWindowString(file, windowStart, position, readBlock[:])
			if windowErr != nil {
				return "", windowErr
			}
			return selectAdminLogTailFromWindow(file, window, windowStart, maxLines)
		}

		data := block
		if len(pending) > 0 {
			data = make([]byte, len(block)+len(pending))
			copy(data, block)
			copy(data[len(block):], pending)
		}
		end := len(data)
		for end > 0 && len(lines) < maxLines {
			newline := bytes.LastIndexByte(data[:end], '\n')
			if newline < 0 {
				break
			}
			sawNewline = true
			line := data[newline+1 : end]
			if len(bytes.TrimSpace(line)) > 0 {
				lines = append(lines, line)
				contentBytes += len(line)
			}
			end = newline
		}
		pending = data[:end]
		position = start
	}

	// pending 是窗口最左侧尚未遇到换行符的一段。文件开头或完整行边界可以
	// 直接保留；窗口从超长行中间开始时，只有窗口内完全没有换行符才保留，
	// 与原先“单个超长行显示有界后缀”的行为一致。
	if len(lines) < maxLines && len(bytes.TrimSpace(pending)) > 0 {
		includePending := windowStart == 0 || !sawNewline
		if !includePending && windowStart > 0 {
			var previous [1]byte
			if n, previousErr := file.ReadAt(previous[:], windowStart-1); n == 1 && previousErr == nil {
				includePending = previous[0] == '\n'
			}
		}
		if includePending {
			lines = append(lines, pending)
			contentBytes += len(pending)
		}
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	var result strings.Builder
	result.Grow(contentBytes + max(0, len(lines)-1))
	for index, line := range lines {
		if index > 0 {
			result.WriteByte('\n')
		}
		_, _ = result.Write(line)
	}
	return result.String(), nil
}

func readAdminLogWindowString(file *os.File, start, end int64, scratch []byte) (string, error) {
	if end <= start {
		return "", nil
	}
	var content strings.Builder
	content.Grow(int(end - start))
	position := start
	for position < end {
		chunkSize := int64(len(scratch))
		if remaining := end - position; remaining < chunkSize {
			chunkSize = remaining
		}
		n, err := file.ReadAt(scratch[:int(chunkSize)], position)
		if n > 0 {
			_, _ = content.Write(scratch[:n])
			position += int64(n)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if n == 0 || errors.Is(err, io.EOF) {
			break
		}
	}
	return content.String(), nil
}

func selectAdminLogTailFromWindow(
	file *os.File,
	window string,
	windowStart int64,
	maxLines int,
) (string, error) {
	lines := make([]string, 0, min(maxLines, 64))
	contentBytes := 0
	end := len(window)
	sawNewline := false
	for end > 0 && len(lines) < maxLines {
		newline := strings.LastIndexByte(window[:end], '\n')
		if newline < 0 {
			break
		}
		sawNewline = true
		line := window[newline+1 : end]
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
			contentBytes += len(line)
		}
		end = newline
	}

	pending := window[:end]
	if len(lines) < maxLines && strings.TrimSpace(pending) != "" {
		includePending := windowStart == 0 || !sawNewline
		if !includePending && windowStart > 0 {
			var previous [1]byte
			n, err := file.ReadAt(previous[:], windowStart-1)
			if err != nil && !errors.Is(err, io.EOF) {
				return "", err
			}
			includePending = n == 1 && previous[0] == '\n'
		}
		if includePending {
			lines = append(lines, pending)
			contentBytes += len(pending)
		}
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	if len(lines) == 0 {
		return "", nil
	}
	if len(lines) == 1 {
		return lines[0], nil
	}
	var result strings.Builder
	result.Grow(contentBytes + len(lines) - 1)
	for index, line := range lines {
		if index > 0 {
			result.WriteByte('\n')
		}
		result.WriteString(line)
	}
	return result.String(), nil
}
