// login.go — WorkBuddy OAuth 登录（设备授权流程，支持 CN 及 Global 国际版）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login url [--global|-g]   → POST /v2/plugin/auth/state?platform=CLI 拿 state+authUrl，
//	                            state 落临时文件，stdout 打印授权 URL
//	login poll                → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	                            成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	                            stdout 打印完整 token+account JSON
//
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 上游常量（CN 与 Global）
const (
	upstreamBaseCN      = "https://copilot.tencent.com"
	originRefererCN     = "https://www.codebuddy.cn"
	upstreamBaseGlobal  = "https://www.workbuddy.ai"
	originRefererGlobal = "https://www.workbuddy.ai"
	cliVersion          = "2.149.0"
	clientUA            = "CLI/" + cliVersion + " CodeBuddy/" + cliVersion
)

var stateFile = filepath.Join(os.TempDir(), "wb2api-login-state.json")

// commonHeaders 通用请求头
func commonHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if origin == "" {
		origin = originRefererCN
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// apiEnvelope 响应信封
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发送 JSON 请求：{code,msg,data} 信封，code!=0 → error
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader, origin string) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req, origin)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

type loginState struct {
	State        string `json:"state"`
	Region       string `json:"region"`
	UpstreamBase string `json:"upstream_base"`
	Origin       string `json:"origin"`
	Proxy        string `json:"proxy,omitempty"`
}

func parseRegion(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-g" || arg == "--global" || arg == "global" {
			return "global"
		}
		if (arg == "--region" || arg == "-r") && i+1 < len(args) {
			if strings.ToLower(args[i+1]) == "global" {
				return "global"
			}
		}
		if strings.HasPrefix(arg, "--region=") {
			if strings.ToLower(strings.TrimPrefix(arg, "--region=")) == "global" {
				return "global"
			}
		}
	}
	return "cn"
}

func parseProxy(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if (arg == "--proxy" || arg == "-p") && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "--proxy=") {
			return strings.TrimPrefix(arg, "--proxy=")
		}
	}
	if v := os.Getenv("WB2A_PROXY"); v != "" {
		return v
	}
	return ""
}

func makeHTTPClient(proxyURL string, jar http.CookieJar) *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	if proxyURL != "" {
		trimmed := strings.TrimSpace(proxyURL)
		if !strings.Contains(trimmed, "://") {
			trimmed = "http://" + trimmed
		}
		if u, err := url.Parse(trimmed); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: 30 * time.Second, Jar: jar, Transport: tr}
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|poll> [--global|-g] [--proxy <url>]")
	}
	// 每个流程独立 cookie jar（多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)

	switch os.Args[1] {
	case "url":
		region := parseRegion(os.Args[2:])
		proxyURL := parseProxy(os.Args[2:])
		client := makeHTTPClient(proxyURL, jar)
		upstreamBase := upstreamBaseCN
		origin := originRefererCN
		if region == "global" {
			upstreamBase = upstreamBaseGlobal
			origin = originRefererGlobal
		}
		endpointAuthState := upstreamBase + "/v2/plugin/auth/state?platform=CLI"

		data, _, err := doJSON(client, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")), origin)
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
			fatal("auth state: missing state or authUrl")
		}
		raw, _ := json.Marshal(loginState{
			State:        st.State,
			Region:       region,
			UpstreamBase: upstreamBase,
			Origin:       origin,
			Proxy:        proxyURL,
		})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(st.AuthURL)

	case "poll":
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			fatal("read state: %v (先跑 login url)", err)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("parse state: %v", err)
		}
		if ls.UpstreamBase == "" {
			ls.UpstreamBase = upstreamBaseCN
			ls.Origin = originRefererCN
			ls.Region = "cn"
		}
		client := makeHTTPClient(ls.Proxy, jar)
		endpointAuthToken := ls.UpstreamBase + "/v2/plugin/auth/token?state="
		endpointLoginAcct := ls.UpstreamBase + "/v2/plugin/login/account?state="

		tokRaw, status, errTok := doJSON(client, http.MethodGet, endpointAuthToken+ls.State, nil, nil, ls.Origin)
		if errTok != nil {
			if status == 0 || status >= 500 {
				fatal("token endpoint error: %v", errTok)
			}
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		if tok.Domain == "" && ls.Region == "global" {
			tok.Domain = "www.workbuddy.ai"
		}
		// login/account 拿 uid/nickname（带 Bearer）
		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		acctHeaders := func(r *http.Request) {
			commonHeaders(r, ls.Origin)
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
		if acctRaw, _, errAcct := doJSON(client, http.MethodGet, endpointLoginAcct+ls.State, acctHeaders, nil, ls.Origin); errAcct == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}
		out := map[string]any{
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"expires_in":    tok.ExpiresIn,
			"domain":        tok.Domain,
			"uid":           acct.UID,
			"enterprise_id": acct.EnterpriseID,
			"nickname":      acct.Nickname,
			"region":        ls.Region,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
	}
}
