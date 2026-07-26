package admin

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// 内容安全策略不含 script-src 'unsafe-inline'，因此任何内联事件处理器
// （onclick= 等）在浏览器中都会被静默拦截——按钮看起来正常但点了没反应。
// 交互必须一律走 data-*-action 委派（见 assets/utils.js）。
var inlineHandlerPattern = regexp.MustCompile(`(?i)\son(click|change|input|submit|load|error|keydown|keyup|focus|blur)\s*=\s*["']`)

// 提取 data-click/change/input-action="名字"，跳过含模板插值 ${...} 的动态值。
var actionUsePattern = regexp.MustCompile(`data-(?:click|change|input)-action="([^"${]+)"`)

// 匹配 registerActions({ ... }) 块内的键：'kebab-case': function 或 camelCase: function。
var (
	registerBlockPattern = regexp.MustCompile(`(?s)registerActions\(\{(.*?)\}\);`)
	actionKeyPattern     = regexp.MustCompile(`(?:'([\w-]+)'|([A-Za-z_$][\w$]*))\s*:\s*function`)
)

func readAssetFiles(t *testing.T, suffixes ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(Assets, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		for _, suffix := range suffixes {
			if strings.HasSuffix(path, suffix) {
				data, readErr := Assets.ReadFile(path)
				if readErr != nil {
					return readErr
				}
				out[path] = string(data)
				return nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("未找到任何 %v 资源文件", suffixes)
	}
	return out
}

// TestNoInlineEventHandlers 确保前端资源里没有内联事件处理器。
// 新增一个 onclick= 会让对应按钮在严格 CSP 下静默失效，这里直接拦下。
func TestNoInlineEventHandlers(t *testing.T) {
	for path, content := range readAssetFiles(t, ".html", ".js") {
		for i, line := range strings.Split(content, "\n") {
			if match := inlineHandlerPattern.FindString(line); match != "" {
				t.Errorf(
					"%s:%d 存在内联事件处理器 %q；CSP 会拦截它，请改用 data-*-action 委派",
					path, i+1, strings.TrimSpace(match),
				)
			}
		}
	}
}

// TestEveryActionIsRegistered 确保每个 data-*-action 都有对应的处理函数。
// 名字写错时按钮不会报错，只是毫无反应——必须在 CI 阶段发现。
func TestEveryActionIsRegistered(t *testing.T) {
	assets := readAssetFiles(t, ".html", ".js")

	used := map[string]string{} // action -> 首次出现的文件
	for path, content := range assets {
		for _, m := range actionUsePattern.FindAllStringSubmatch(content, -1) {
			if _, seen := used[m[1]]; !seen {
				used[m[1]] = path
			}
		}
	}
	if len(used) == 0 {
		t.Fatal("未发现任何 data-*-action，事件委派可能被整体移除了")
	}

	registered := map[string]bool{}
	for path, content := range assets {
		if !strings.HasSuffix(path, ".js") {
			continue
		}
		for _, block := range registerBlockPattern.FindAllStringSubmatch(content, -1) {
			for _, key := range actionKeyPattern.FindAllStringSubmatch(block[1], -1) {
				name := key[1]
				if name == "" {
					name = key[2]
				}
				registered[name] = true
			}
		}
	}

	for action, path := range used {
		if !registered[action] {
			t.Errorf("%s 使用了 action %q，但没有任何 registerActions 注册它（点击将无反应）", path, action)
		}
	}
	for action := range registered {
		if _, ok := used[action]; !ok {
			t.Errorf("action %q 已注册但无人使用，可能是迁移遗留或拼写不一致", action)
		}
	}
}

// TestActionDelegationHelperIsPresent 锁定委派机制本身存在。
func TestActionDelegationHelperIsPresent(t *testing.T) {
	utils, err := Assets.ReadFile("assets/utils.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		"function registerActions",
		"function dispatchAction",
		"document.addEventListener('click'",
		"document.addEventListener('change'",
		"document.addEventListener('input'",
	} {
		if !strings.Contains(string(utils), token) {
			t.Errorf("utils.js 缺少事件委派组件 %q", token)
		}
	}
}
