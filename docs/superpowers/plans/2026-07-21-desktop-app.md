# calsync デスクトップアプリ v1 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tauri v2 + Go サイドカー(`calsync appserver`)構成の macOS デスクトップアプリを追加し、構成俯瞰・設定フォーム編集・アカウント追加・デーモン制御を GUI で行えるようにする。

**Architecture:** 機能ロジックは全て Go の新パッケージ `internal/appserver`(127.0.0.1 限定 HTTP JSON API)に置き、Tauri の Rust 殻はサイドカーの spawn/kill のみを行う薄い層に保つ。フロントは React + TypeScript + Vite で、起動時にサイドカーの stdout から `{"port","token"}` ハンドシェイクを受けて Bearer トークン付き fetch で叩く。仕様は `docs/superpowers/specs/2026-07-21-desktop-app-design.md`。

**Tech Stack:** Go 1.25(既存)/ cobra / yaml.v3 / google.golang.org/api(既存)、Tauri 2.x + tauri-plugin-shell + tauri-plugin-dialog、React 18 + Vite + vitest。

## Global Constraints

- デーモン本体のビルドは CGO 不要のまま(`go build -o ./calsync ./cmd/calsync` が Rust/Node なしで通ること)
- テストは必ず `go test ./... -race -count=1`。`go vet ./...` と `gofmt -l internal/ cmd/`(出力なし)も毎タスク通す
- `go mod tidy` 禁止。依存追加は対象限定の `go get <module>@<version>`(今回は新規 Go 依存なしの見込み)
- コミットは Conventional Commits(英語)。コミットメッセージのトレーラーは実行環境の指示に従う
- 公開リポジトリ: 実環境識別子(実在メールアドレス・実在アカウント id・カレンダー ID・個人 URL)をコード・テスト・ドキュメントに書かない。例示は `personal` / `work-ms` / `user@gmail.com` 形式
- 図を書く場合は Mermaid のみ
- SQLite へのアクセスは `store.OpenReadOnly` のみ(書き込みオープン禁止)。launchd 検出成功時以外は DB ファイルに触れない
- launchd ラベルは `com.btajp.calsync`、plist は `~/Library/LaunchAgents/com.btajp.calsync.plist`、データ配置は `<data>/calsync.yaml`(install-launchd.sh の既定)

---

### Task 1: config.Parse の切り出しと Raw 型の公開

`config.Load` からバイト列パース部を `Parse` として切り出し、appserver が「ファイルに書く前のバイト列」を検証できるようにする。同時に `rawConfig` 系を `Raw` 系にリネームして公開し、JSON タグ(yaml と同名)を付ける(appserver の GET/PUT ボディに使うため)。

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`(追記)

**Interfaces:**
- Produces: `config.Parse(data []byte, source string) (*Config, error)`(source はエラーメッセージ用の表示名)
- Produces: 公開型 `config.Raw` / `RawNotifications` / `RawSlack` / `RawProviders` / `RawGoogleProvider` / `RawMicrosoftProvider` / `RawAccount` / `RawDetailSync`(全フィールドに `json:"<yaml名>"` タグ追加。`Raw.DedupeSameMeeting` は `*bool` のまま)
- `Load` の挙動・エラーメッセージは現状維持(`config: parse <path>: ...` の `<path>` は source で出す)

- [ ] **Step 1: 失敗するテストを書く**

`internal/config/config_test.go` に追記:

```go
func TestParseBytes(t *testing.T) {
	yamlSrc := []byte(`
providers:
  google: {credentials_file: /tmp/creds.json}
accounts:
  - {id: personal, provider: google}
  - {id: work-ms, provider: microsoft}
`)
	cfg, err := Parse(yamlSrc, "inline")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Accounts) != 2 || cfg.Accounts[0].ID != "personal" {
		t.Fatalf("unexpected accounts: %+v", cfg.Accounts)
	}
	// 不正値は source 名入りで拒否される
	if _, err := Parse([]byte("poll_interval: banana"), "inline"); err == nil {
		t.Fatal("want error for invalid poll_interval")
	}
	// Raw に JSON タグが付いている(API ボディ互換の要)
	b, _ := json.Marshal(Raw{PollInterval: "1m"})
	if !strings.Contains(string(b), `"poll_interval"`) {
		t.Fatalf("Raw json tags missing: %s", b)
	}
}
```

(`encoding/json`・`strings` を import に追加)

- [ ] **Step 2: 失敗を確認** — Run: `go test ./internal/config/ -run TestParseBytes -count=1`。Expected: FAIL(`undefined: Parse` / `undefined: Raw`)

- [ ] **Step 3: 実装**

`config.go` の変更点:

1. `rawConfig`→`Raw`、`rawNotifications`→`RawNotifications`、`rawSlack`→`RawSlack`、`rawProviders`→`RawProviders`、`rawGoogleProvider`→`RawGoogleProvider`、`rawMicrosoftProvider`→`RawMicrosoftProvider`、`rawAccount`→`RawAccount`、`rawDetailSync`→`RawDetailSync` に一括リネーム(ファイル内参照も)
2. 各フィールドに json タグを追加。例: `PollInterval string \`yaml:"poll_interval" json:"poll_interval,omitempty"\``。全フィールド同様に yaml 名と同一の json 名 + `omitempty`(`RawAccount.ID` など必須系too — omitempty で問題ない。`DedupeSameMeeting *bool` は `json:"dedupe_same_meeting,omitempty"`)
3. `Load` を分割:

```go
// Load は YAML 設定を読み込み、検証とデフォルト補完を行う。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(b, path)
}

// Parse は YAML バイト列を検証・デフォルト補完して Config にする。
// source はエラーメッセージに使う表示名(ファイルパス等)。
func Parse(data []byte, source string) (*Config, error) {
	var raw Raw
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // 未知キーはエラー(タイポの黙殺を防ぐ)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", source, err)
	}
	// …以降、既存 Load の本体をそのまま移動(path 参照箇所は source に置換)…
}
```

(`bytes` を import。既存 Load 本体の検証ロジックは一切変更しない)

- [ ] **Step 4: 通過確認** — Run: `go test ./internal/config/ -count=1` → PASS、`go test ./... -race -count=1` → 全 PASS(リネーム漏れはここで検出)
- [ ] **Step 5: `go vet ./... && gofmt -l internal/ cmd/`(出力なし)を確認してコミット** — `refactor(config): extract Parse and export Raw types for appserver`

---

### Task 2: internal/clients パッケージ(OAuth 設定・プロバイダ構築の共有化)

`cmd/calsync/main.go` の `oauthConfigFor` / `buildProvider` を新パッケージへ移設し、appserver から使えるようにする。Google のスコープに CalendarList 読み取りを追加する。

**Files:**
- Create: `internal/clients/clients.go`
- Test: `internal/clients/clients_test.go`
- Modify: `cmd/calsync/main.go`(本体を移設し、薄い委譲ラッパーを残す)

**Interfaces:**
- Produces: `clients.OAuthConfigFor(cfg *config.Config, acct config.Account) (*oauth2.Config, error)`
- Produces: `clients.BuildProvider(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (provider.Provider, error)`
- Produces: `clients.BuildGoogleClient(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (*google.Client, error)`(カレンダーリスト用に具象型を返す版)
- `cmd/calsync/main.go` には `func oauthConfigFor(...)` / `func buildProvider(...)` を委譲ラッパーとして残す(`cli_test.go` を変更しないため)

- [ ] **Step 1: 失敗するテストを書く**

`internal/clients/clients_test.go`:

```go
package clients

import (
	"strings"
	"testing"

	"github.com/btajp/calsync/internal/config"
)

func TestOAuthConfigForMicrosoft(t *testing.T) {
	cfg := &config.Config{}
	cfg.Providers.Microsoft.ClientID = "client-id-value"
	oc, err := OAuthConfigFor(cfg, config.Account{ID: "work-ms", Provider: "microsoft"})
	if err != nil {
		t.Fatalf("OAuthConfigFor: %v", err)
	}
	// 「localhost・パスなし」のリダイレクト形式(不変条件)
	if oc.RedirectURL != "http://localhost" {
		t.Fatalf("redirect = %q", oc.RedirectURL)
	}
}

func TestOAuthConfigForGoogleScopes(t *testing.T) {
	// credentials ファイルを一時生成
	dir := t.TempDir()
	credPath := dir + "/creds.json"
	credJSON := `{"installed":{"client_id":"x","client_secret":"y","redirect_uris":["http://localhost"],"auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token"}}`
	if err := os.WriteFile(credPath, []byte(credJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Providers.Google.CredentialsFile = credPath
	oc, err := OAuthConfigFor(cfg, config.Account{ID: "personal", Provider: "google"})
	if err != nil {
		t.Fatalf("OAuthConfigFor: %v", err)
	}
	joined := strings.Join(oc.Scopes, " ")
	if !strings.Contains(joined, "calendar.events") || !strings.Contains(joined, "calendar.calendarlist.readonly") {
		t.Fatalf("scopes = %v", oc.Scopes)
	}
}
```

(`os` を import)

- [ ] **Step 2: 失敗を確認** — Run: `go test ./internal/clients/ -count=1`。Expected: FAIL(パッケージ未作成)
- [ ] **Step 3: 実装**

`internal/clients/clients.go`: main.go から `oauthConfigFor`・`buildProvider` の本体を移動(コメントも保持)。変更点は 2 つだけ:

1. Google の scope 引数を `googleoauth.ConfigFromJSON(b, "https://www.googleapis.com/auth/calendar.events", "https://www.googleapis.com/auth/calendar.calendarlist.readonly")` に変更し、直上に理由コメントを追加: `// calendarlist.readonly はデスクトップアプリのカレンダー選択 UI 用。refresh には影響しない(既存トークンは再認可まで list 不可)。`
2. `BuildGoogleClient` を追加:

```go
// BuildGoogleClient は google 用の具象クライアントを返す(CalendarList 用)。
// provider.Provider インターフェースには CalendarList が無いため具象型を返す。
func BuildGoogleClient(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (*google.Client, error) {
	if acct.Provider != "google" {
		return nil, fmt.Errorf("account %s: provider %q does not support calendar listing", acct.ID, acct.Provider)
	}
	tok, err := tokens.Load(acct.ID)
	if err != nil {
		return nil, fmt.Errorf("account %s: no token: %w", acct.ID, err)
	}
	ocfg, err := OAuthConfigFor(cfg, acct)
	if err != nil {
		return nil, err
	}
	ts := auth.PersistingTokenSource(acct.ID, tokens, ocfg.TokenSource(context.Background(), tok))
	return google.New(ts, acct.ID), nil
}
```

`cmd/calsync/main.go` は本体を消して委譲ラッパーに置換:

```go
func oauthConfigFor(cfg *config.Config, acct config.Account) (*oauth2.Config, error) {
	return clients.OAuthConfigFor(cfg, acct)
}

func buildProvider(cfg *config.Config, tokens *auth.TokenStore, acct config.Account) (provider.Provider, error) {
	return clients.BuildProvider(cfg, tokens, acct)
}
```

(不要になった import を整理。`googleoauth` 等は clients 側へ移る)

- [ ] **Step 4: 通過確認** — `go test ./internal/clients/ -count=1` → PASS、`go test ./... -race -count=1` → 全 PASS(cli_test.go が委譲ラッパー経由で通ること)
- [ ] **Step 5: vet/gofmt 確認してコミット** — `refactor(clients): extract oauth config and provider builders for appserver reuse`

---

### Task 3: internal/doctor パッケージ抽出

`runDoctor` / `probeFunc` / `findOrphanAccounts` を `internal/doctor` へ移設する(appserver の `/api/doctor` から使うため)。

**Files:**
- Create: `internal/doctor/doctor.go`
- Test: `internal/doctor/doctor_test.go`
- Modify: `cmd/calsync/cmd_doctor.go`・`cmd/calsync/main.go`(委譲ラッパー化)

**Interfaces:**
- Produces: `doctor.Probe`(= `func(ctx context.Context, acct config.Account) error`)
- Produces: `doctor.Run(ctx context.Context, cfg *config.Config, dataDir string, probe Probe, out io.Writer, configPath string) error`
- Produces: `doctor.FindOrphanAccounts(cfgIDs, dbIDs []string) []string`
- cmd 側は `type probeFunc = doctor.Probe`・`runDoctor` → `doctor.Run` 委譲・`findOrphanAccounts` → `doctor.FindOrphanAccounts` 委譲(cli_test.go 不変)

- [ ] **Step 1: 失敗するテストを書く** — `internal/doctor/doctor_test.go`:

```go
package doctor

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btajp/calsync/internal/config"
)

func TestRunReportsMissingToken(t *testing.T) {
	dir := t.TempDir() // tokens/ が無い = 全アカウント token MISSING
	cfg := &config.Config{Accounts: []config.Account{{ID: "personal", Provider: "google"}}}
	var out bytes.Buffer
	probe := func(ctx context.Context, acct config.Account) error { return errors.New("should not be called") }
	err := Run(context.Background(), cfg, dir, probe, &out, "calsync.yaml")
	if err == nil {
		t.Fatal("want problem error")
	}
	if !bytes.Contains(out.Bytes(), []byte("token MISSING")) {
		t.Fatalf("out = %s", out.String())
	}
}

func TestFindOrphanAccounts(t *testing.T) {
	got := FindOrphanAccounts([]string{"personal"}, []string{"personal", "old-acct", "old-acct"})
	if len(got) != 1 || got[0] != "old-acct" {
		t.Fatalf("got %v", got)
	}
}
```

- [ ] **Step 2: 失敗を確認** — `go test ./internal/doctor/ -count=1` → FAIL(パッケージ未作成)
- [ ] **Step 3: 実装** — `cmd_doctor.go` の `runDoctor`(コメント含む)と `main.go` の `findOrphanAccounts` を `internal/doctor/doctor.go` へ移動し、名前を `Run` / `FindOrphanAccounts` / `Probe` に変更。cmd 側は委譲ラッパー(`runDoctor` は `doctor.Run` を呼ぶ 1 行、`probeFunc` は型エイリアス)に置換
- [ ] **Step 4: 通過確認** — `go test ./... -race -count=1` → 全 PASS
- [ ] **Step 5: vet/gofmt 確認してコミット** — `refactor(doctor): extract doctor checks into internal/doctor`

---

### Task 4: google.Client.ListCalendars

Google の CalendarList 取得メソッドを追加する(アカウント追加ウィザードのカレンダー選択用)。

**Files:**
- Modify: `internal/provider/google/google.go`
- Test: `internal/provider/google/calendarlist_test.go`(新規・in-package)

**Interfaces:**
- Produces:

```go
type CalendarListEntry struct {
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	Primary    bool   `json:"primary"`
	AccessRole string `json:"access_role"`
}
func (c *Client) ListCalendars(ctx context.Context) ([]CalendarListEntry, error)
```

- [ ] **Step 1: 失敗するテストを書く** — `calendarlist_test.go`(既存テストの `httptest` + `baseURL` 差し替えパターンに合わせる):

```go
package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListCalendars(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me/calendarList" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if page == 0 {
			page++
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "primary-cal-id", "summary": "Main", "primary": true, "accessRole": "owner"},
				},
				"nextPageToken": "p2",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"id": "team-cal-id@group.calendar.google.com", "summary": "Team", "accessRole": "reader"},
			},
		})
	}))
	defer srv.Close()

	c := New(nil, "personal")
	c.baseURL = srv.URL
	got, err := c.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(got) != 2 || !got[0].Primary || got[1].ID != "team-cal-id@group.calendar.google.com" {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 2: 失敗を確認** — `go test ./internal/provider/google/ -run TestListCalendars -count=1` → FAIL(`undefined: CalendarListEntry`)
- [ ] **Step 3: 実装** — `google.go` に追加(ページネーションあり・リトライは既存 busy 系と同様の扱いにせず素直に 1 回。認可エラーの正規化は呼び出し側で行う):

```go
// ListCalendars はアカウントの CalendarList 全件を返す(デスクトップアプリの
// カレンダー選択 UI 用)。要スコープ calendar.calendarlist.readonly。
func (c *Client) ListCalendars(ctx context.Context) ([]CalendarListEntry, error) {
	svc, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	var out []CalendarListEntry
	call := svc.CalendarList.List().MaxResults(250)
	for {
		res, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("google calendar list (%s): %w", c.accountID, err)
		}
		for _, it := range res.Items {
			out = append(out, CalendarListEntry{ID: it.Id, Summary: it.Summary, Primary: it.Primary, AccessRole: it.AccessRole})
		}
		if res.NextPageToken == "" {
			return out, nil
		}
		call = svc.CalendarList.List().MaxResults(250).PageToken(res.NextPageToken)
	}
}
```

- [ ] **Step 4: 通過確認** — `go test ./internal/provider/google/ -count=1` → PASS
- [ ] **Step 5: vet/gofmt 確認してコミット** — `feat(google): add CalendarList listing for the desktop app`

---

### Task 5: internal/appserver コア(Server・認証・ハンドシェイク・status・デーモン検出)

appserver の骨格を作る。トークン認証ミドルウェア・`{"port","token"}` ハンドシェイク・stdin EOF 監視・`GET /api/status`・launchd/コンテナ検出。

**Files:**
- Create: `internal/appserver/server.go`(Server・New・Handler・Serve・認証)
- Create: `internal/appserver/daemon.go`(CmdRunner・detectDaemon)
- Create: `internal/appserver/status.go`(GET /api/status)
- Test: `internal/appserver/server_test.go`

**Interfaces:**
- Produces:

```go
type CmdRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}
type DaemonInfo struct {
	Mode    string `json:"mode"` // "launchd" | "manual" | "container" | "unknown"
	Running bool   `json:"running"`
	Detail  string `json:"detail,omitempty"`
}
type Server struct {
	ConfigPath, DataDir, Token string
	Runner   CmdRunner // 既定: exec.CommandContext ベース
	UID      int       // launchctl gui ドメイン用。既定 os.Getuid()
	PlistPath string   // 既定 ~/Library/LaunchAgents/com.btajp.calsync.plist
	// RunFlow / ListCals / Probe は後続タスクで追加
}
func New(configPath, dataDir, token string) *Server
func (s *Server) Handler() http.Handler
// Serve は ln で HTTP を提供し、開始直後に {"port":N,"token":"..."} を out に 1 行 JSON で書く。
// ctx キャンセルで graceful shutdown。
func (s *Server) Serve(ctx context.Context, ln net.Listener, out io.Writer) error
// WatchStdinEOF は r の EOF(親プロセス死亡)で cancel を呼ぶゴルーチンを起動する。
func WatchStdinEOF(r io.Reader, cancel context.CancelFunc)
func writeErr(w http.ResponseWriter, status int, code, message, hint string) // {"code","message","hint"}
```

- `GET /api/status` レスポンス:

```go
type TokenStatus struct {
	AccountID string `json:"account_id"`
	State     string `json:"state"` // "ok" | "missing" | "no_refresh_token"
}
type CalendarStatus struct {
	AccountID  string `json:"account_id"`
	CalendarID string `json:"calendar_id"`
	LastSync   string `json:"last_sync"` // RFC3339 or ""
	Status     string `json:"status"`    // "ok" or エラー文字列
}
type StatusResponse struct {
	Daemon    DaemonInfo       `json:"daemon"`
	Tokens    []TokenStatus    `json:"tokens"`
	Calendars []CalendarStatus `json:"calendars,omitempty"`
	DBNote    string           `json:"db_note,omitempty"` // DB を読まなかった/読めなかった理由
}
```

- [ ] **Step 1: 失敗するテストを書く** — `server_test.go`:

```go
package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner は launchctl / docker 呼び出しを台本で返す。
type fakeRunner struct{ outputs map[string]struct{ out string; err error } }

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	key := name + " " + strings.Join(args, " ")
	if r, ok := f.outputs[key]; ok {
		return r.out, "", r.err
	}
	return "", "", fmt.Errorf("unexpected command: %s", key)
}

func testServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "calsync.yaml")
	os.WriteFile(cfgPath, []byte(`
providers:
  google: {credentials_file: /tmp/creds.json}
accounts:
  - {id: personal, provider: google}
`), 0o600)
	s := New(cfgPath, dir, "test-token")
	s.PlistPath = filepath.Join(dir, "no-such.plist") // 既定: launchd 未検出
	s.Runner = &fakeRunner{outputs: map[string]struct{ out string; err error }{}}
	return s, dir
}

func TestAuthRequired(t *testing.T) {
	s, _ := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/status")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func get(t *testing.T, srv *httptest.Server, token, path string, into any) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if into != nil {
		defer res.Body.Close()
		if err := json.NewDecoder(res.Body).Decode(into); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return res
}

func TestStatusManualMode(t *testing.T) {
	s, _ := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got StatusResponse
	res := get(t, srv, "test-token", "/api/status", &got)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got.Daemon.Mode != "manual" {
		t.Fatalf("mode = %q", got.Daemon.Mode)
	}
	// launchd 未検出時は DB に触れない(不変条件)
	if got.DBNote == "" || len(got.Calendars) != 0 {
		t.Fatalf("expected db skip note, got %+v", got)
	}
	// トークン状態はファイルベースなのでどのモードでも返る
	if len(got.Tokens) != 1 || got.Tokens[0].State != "missing" {
		t.Fatalf("tokens = %+v", got.Tokens)
	}
}

func TestStatusContainerGuard(t *testing.T) {
	s, _ := testServer(t)
	s.Runner = &fakeRunner{outputs: map[string]struct{ out string; err error }{
		"docker ps --format {{.Names}}": {out: "calsync\nother\n"},
	}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got StatusResponse
	get(t, srv, "test-token", "/api/status", &got)
	if got.Daemon.Mode != "container" {
		t.Fatalf("mode = %q", got.Daemon.Mode)
	}
}

func TestStatusLaunchdRunning(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	os.WriteFile(plist, []byte("<plist/>"), 0o600)
	s.PlistPath = plist
	s.UID = 501
	s.Runner = &fakeRunner{outputs: map[string]struct{ out string; err error }{
		"launchctl print gui/501/com.btajp.calsync": {out: "state = running\n"},
	}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got StatusResponse
	get(t, srv, "test-token", "/api/status", &got)
	if got.Daemon.Mode != "launchd" || !got.Daemon.Running {
		t.Fatalf("daemon = %+v", got.Daemon)
	}
	// DB 未作成は正常系(db_note で伝える)
	if got.DBNote == "" {
		t.Fatalf("want db_note for missing db, got %+v", got)
	}
}

func TestServeHandshake(t *testing.T) {
	s, _ := testServer(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, &out) }()
	// ハンドシェイク行が出るまで少し待つ
	deadline := time.Now().Add(2 * time.Second)
	for out.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var hs struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &hs); err != nil {
		t.Fatalf("handshake %q: %v", out.String(), err)
	}
	if hs.Token != "test-token" || hs.Port == 0 {
		t.Fatalf("handshake = %+v", hs)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
```

(`net`・`time` を import)

- [ ] **Step 2: 失敗を確認** — `go test ./internal/appserver/ -count=1` → FAIL(パッケージ未作成)
- [ ] **Step 3: 実装**

`server.go`:

```go
// Package appserver はデスクトップアプリ向けの localhost 限定 HTTP API を提供する。
// 不変条件: SQLite は OpenReadOnly のみ・launchd 検出時のみ。書き込みはファイル
// (YAML・トークン)に限る。認証は起動ごとのワンタイム Bearer トークン。
package appserver

type Server struct {
	ConfigPath string
	DataDir    string
	Token      string
	Runner     CmdRunner
	UID        int
	PlistPath  string
}

func New(configPath, dataDir, token string) *Server {
	home, _ := os.UserHomeDir()
	return &Server{
		ConfigPath: configPath,
		DataDir:    dataDir,
		Token:      token,
		Runner:     execRunner{},
		UID:        os.Getuid(),
		PlistPath:  filepath.Join(home, "Library", "LaunchAgents", "com.btajp.calsync.plist"),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	return s.requireToken(mux)
}

// requireToken は Bearer トークン一致以外を一律 401 にする。
// 比較は subtle.ConstantTimeCompare(タイミング攻撃対策)。
func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or missing token", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, code, message, hint string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Code: code, Message: message, Hint: hint})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener, out io.Writer) error {
	hs, _ := json.Marshal(map[string]any{"port": ln.Addr().(*net.TCPAddr).Port, "token": s.Token})
	fmt.Fprintln(out, string(hs))
	srv := &http.Server{Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// WatchStdinEOF は親(Tauri 殻)の死亡を stdin の EOF で検知して cancel を呼ぶ。
// サイドカーの孤児化防止の最後の砦(仕様 §5)。
func WatchStdinEOF(r io.Reader, cancel context.CancelFunc) {
	go func() {
		_, _ = io.Copy(io.Discard, r)
		cancel()
	}()
}

// GenerateToken は起動ごとのワンタイムトークンを作る(32 バイト乱数の hex)。
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
```

`daemon.go`:

```go
type CmdRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	var so, se bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	return so.String(), se.String(), err
}

const launchdLabel = "com.btajp.calsync"

type DaemonInfo struct {
	Mode    string `json:"mode"`
	Running bool   `json:"running"`
	Detail  string `json:"detail,omitempty"`
}

// detectDaemon は運用形態を判定する。
// plist あり → launchd 管理(launchctl print で稼働判定)。
// plist なし → docker で calsync コンテナ稼働中なら container(全 DB アクセス禁止)、
// でなければ manual(手動運用 or 未セットアップ。DB には触らない)。
func (s *Server) detectDaemon(ctx context.Context) DaemonInfo {
	if _, err := os.Stat(s.PlistPath); err == nil {
		target := fmt.Sprintf("gui/%d/%s", s.UID, launchdLabel)
		out, _, err := s.Runner.Run(ctx, "launchctl", "print", target)
		if err != nil {
			return DaemonInfo{Mode: "launchd", Running: false, Detail: "installed but not loaded"}
		}
		return DaemonInfo{Mode: "launchd", Running: strings.Contains(out, "state = running")}
	}
	if _, err := s.LookPath("docker"); err == nil {
		out, _, err := s.Runner.Run(ctx, "docker", "ps", "--format", "{{.Names}}")
		if err == nil {
			for _, name := range strings.Fields(out) {
				if name == "calsync" {
					return DaemonInfo{Mode: "container", Running: true,
						Detail: "container detected: host-side access to data/ is unsafe (VirtioFS)"}
				}
			}
		}
	}
	return DaemonInfo{Mode: "manual", Running: false}
}
```

`Server` に `LookPath func(string) (string, error)` フィールドを追加し、`New` で `LookPath: exec.LookPath` を設定する(docker が PATH に無い CI 環境でもテストが決定的になるように注入可能にする)。テスト `TestStatusContainerGuard` には `s.LookPath = func(string) (string, error) { return "/usr/bin/docker", nil }` を 1 行追加し、`testServer` 既定では `s.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }` を設定して docker 分岐を無効化する。

`status.go`:

```go
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	daemon := s.detectDaemon(r.Context())
	resp := StatusResponse{Daemon: daemon}

	// トークン状態はファイルベース(DB 非依存)なので常に返す
	if cfg, err := config.Load(s.ConfigPath); err == nil {
		tokens := &auth.TokenStore{Dir: s.DataDir}
		for _, acct := range cfg.Accounts {
			st := "ok"
			tok, terr := tokens.Load(acct.ID)
			switch {
			case terr != nil:
				st = "missing"
			case tok.RefreshToken == "":
				st = "no_refresh_token"
			}
			resp.Tokens = append(resp.Tokens, TokenStatus{AccountID: acct.ID, State: st})
		}
	}

	// DB は launchd 検出時のみ・読み取り専用で開く(仕様 §4/§9 の不変条件)
	switch {
	case daemon.Mode != "launchd":
		resp.DBNote = "db skipped: not a launchd-managed setup"
	default:
		if _, err := os.Stat(filepath.Join(s.DataDir, "calsync.db")); os.IsNotExist(err) {
			resp.DBNote = "no local DB yet"
			break
		}
		st, err := store.OpenReadOnly(s.DataDir)
		if err != nil {
			resp.DBNote = "db open failed: " + err.Error()
			break
		}
		defer st.Close()
		states, err := st.ListCalendars()
		if err != nil {
			resp.DBNote = "db read failed: " + err.Error()
			break
		}
		for _, cs := range states {
			last := ""
			if !cs.LastSyncedAt.IsZero() {
				last = cs.LastSyncedAt.UTC().Format(time.RFC3339)
			}
			status := "ok"
			if cs.LastError != "" {
				status = cs.LastError
			}
			resp.Calendars = append(resp.Calendars, CalendarStatus{
				AccountID: cs.Ref.AccountID, CalendarID: cs.Ref.CalendarID,
				LastSync: last, Status: status,
			})
		}
	}
	writeJSON(w, resp)
}
```

- [ ] **Step 4: 通過確認** — `go test ./internal/appserver/ -race -count=1` → PASS
- [ ] **Step 5: vet/gofmt 確認してコミット** — `feat(appserver): add local API server core with status and daemon detection`

---

### Task 6: デーモン制御エンドポイント

`POST /api/daemon/{start|stop|restart}`。launchd モード以外は 409。

**Files:**
- Modify: `internal/appserver/daemon.go`・`server.go`(ルート追加)
- Test: `internal/appserver/daemon_test.go`

**Interfaces:**
- Produces: `POST /api/daemon/start` → `launchctl bootstrap gui/<uid> <PlistPath>`、`POST /api/daemon/stop` → `launchctl bootout gui/<uid>/com.btajp.calsync`、`POST /api/daemon/restart` → `launchctl kickstart -k gui/<uid>/com.btajp.calsync`。成功: `{"ok":true}`。launchd モード外: 409 `{"code":"not_launchd","hint":"launchd 管理外です。手動で操作してください: ..."}`

- [ ] **Step 1: 失敗するテストを書く** — `daemon_test.go`(Task 5 の `testServer`・`fakeRunner` を利用):

```go
func TestDaemonRestart(t *testing.T) {
	s, dir := testServer(t)
	plist := filepath.Join(dir, "com.btajp.calsync.plist")
	os.WriteFile(plist, []byte("<plist/>"), 0o600)
	s.PlistPath = plist
	s.UID = 501
	fr := &fakeRunner{outputs: map[string]struct{ out string; err error }{
		"launchctl print gui/501/com.btajp.calsync":        {out: "state = running\n"},
		"launchctl kickstart -k gui/501/com.btajp.calsync": {out: ""},
	}}
	s.Runner = fr
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/daemon/restart", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
}

func TestDaemonRejectedOutsideLaunchd(t *testing.T) {
	s, _ := testServer(t) // plist なし → manual モード
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/daemon/stop", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d", res.StatusCode)
	}
}
```

- [ ] **Step 2: 失敗を確認** — `go test ./internal/appserver/ -run TestDaemon -count=1` → FAIL(404)
- [ ] **Step 3: 実装** — `daemon.go` に `handleDaemonAction` を追加し、`Handler()` に `mux.HandleFunc("POST /api/daemon/{action}", s.handleDaemonAction)` を登録:

```go
func (s *Server) handleDaemonAction(w http.ResponseWriter, r *http.Request) {
	info := s.detectDaemon(r.Context())
	if info.Mode != "launchd" {
		writeErr(w, http.StatusConflict, "not_launchd",
			"daemon is not managed by launchd on this host",
			"launchd 管理外です。./scripts/macos/install-launchd.sh でのセットアップ、または手動での操作を行ってください")
		return
	}
	target := fmt.Sprintf("gui/%d/%s", s.UID, launchdLabel)
	var args []string
	switch r.PathValue("action") {
	case "start":
		args = []string{"bootstrap", fmt.Sprintf("gui/%d", s.UID), s.PlistPath}
	case "stop":
		args = []string{"bootout", target}
	case "restart":
		args = []string{"kickstart", "-k", target}
	default:
		writeErr(w, http.StatusNotFound, "unknown_action", "unknown daemon action", "")
		return
	}
	if _, stderr, err := s.Runner.Run(r.Context(), "launchctl", args...); err != nil {
		writeErr(w, http.StatusBadGateway, "launchctl_failed", strings.TrimSpace(stderr), "launchctl の失敗です。ログを確認してください")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
```

- [ ] **Step 4: 通過確認** — `go test ./internal/appserver/ -race -count=1` → PASS
- [ ] **Step 5: vet/gofmt 確認してコミット** — `feat(appserver): add launchd daemon control endpoints`

---

### Task 7: 設定の GET/PUT とコメント保持書き戻し

`GET /api/config` / `PUT /api/config`。書き戻しは yaml.v3 Node でコメント移植・検証通過時のみ tmp+rename・`.bak` 1 世代・mtime 競合検出。

**Files:**
- Create: `internal/appserver/yamledit.go`
- Create: `internal/appserver/confighttp.go`
- Test: `internal/appserver/yamledit_test.go`・`internal/appserver/confighttp_test.go`

**Interfaces:**
- Produces(yamledit.go):

```go
var ErrConflict = errors.New("config file changed on disk")
// SaveConfig は raw を YAML 化し、旧ファイルのコメントを移植して path へ
// アトミックに書き戻す。書き込み前に config.Parse で検証し、失敗時はファイル不変。
// baseMtime が現ファイルの ModTime と一致しなければ ErrConflict。
func SaveConfig(path string, raw *config.Raw, baseMtime time.Time) error
// mergeComments は new ツリーへ old ツリーのコメントを移植する。マップはキー名、
// accounts 配列は id、detail_sync 配列は (from,to) で対応付ける。
func mergeComments(oldN, newN *yaml.Node)
```

- Produces(confighttp.go): `GET /api/config` → `{"raw": <config.Raw>, "mtime": "<unixnano 文字列>"}`、`PUT /api/config` ボディ `{"raw": <config.Raw>, "base_mtime": "<unixnano 文字列>"}` → 200 `{"ok":true,"mtime":"<新 unixnano>"}` / 400(検証エラー `{"code":"invalid_config","message":...}`)/ 409(mtime 競合)

- [ ] **Step 1: 失敗するテストを書く** — `yamledit_test.go`:

```go
package appserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btajp/calsync/internal/config"
)

const seedYAML = `# top comment
poll_interval: 1m
providers:
  google:
    credentials_file: /tmp/creds.json # keep me
accounts:
  # personal account comment
  - id: personal
    provider: google
  - id: work-ms
    provider: microsoft
`

func writeSeed(t *testing.T) (string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "calsync.yaml")
	if err := os.WriteFile(p, []byte(seedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	return p, fi.ModTime()
}

func loadRaw(t *testing.T, p string) *config.Raw {
	t.Helper()
	b, _ := os.ReadFile(p)
	var raw config.Raw
	if err := yaml.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	return &raw
}

func TestSaveConfigPreservesComments(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	raw.PollInterval = "2m" // 値を 1 つ変更
	if err := SaveConfig(p, raw, mtime); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	for _, want := range []string{"# top comment", "# keep me", "# personal account comment", "2m"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// .bak に旧内容が残る
	bak, err := os.ReadFile(p + ".bak")
	if err != nil || !strings.Contains(string(bak), "poll_interval: 1m") {
		t.Fatalf("bak: %v %s", err, bak)
	}
}

func TestSaveConfigRejectsInvalid(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	raw.PollInterval = "banana"
	if err := SaveConfig(p, raw, mtime); err == nil {
		t.Fatal("want validation error")
	}
	out, _ := os.ReadFile(p)
	if !strings.Contains(string(out), "poll_interval: 1m") {
		t.Fatal("file must be unchanged on validation failure")
	}
}

func TestSaveConfigConflict(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	if err := SaveConfig(p, raw, mtime.Add(-time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestSaveConfigAddAccountKeepsOtherComments(t *testing.T) {
	p, mtime := writeSeed(t)
	raw := loadRaw(t, p)
	raw.Accounts = append(raw.Accounts, config.RawAccount{ID: "work-a", Provider: "google"})
	if err := SaveConfig(p, raw, mtime); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	if !strings.Contains(s, "# personal account comment") || !strings.Contains(s, "work-a") {
		t.Fatalf("out:\n%s", s)
	}
}
```

(`errors`・`gopkg.in/yaml.v3` を import)

`confighttp_test.go`:

```go
func TestConfigGetPut(t *testing.T) {
	s, dir := testServer(t)
	// testServer の config を seedYAML で上書き
	p := filepath.Join(dir, "calsync.yaml")
	os.WriteFile(p, []byte(seedYAML), 0o600)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	var got struct {
		Raw   config.Raw `json:"raw"`
		Mtime string     `json:"mtime"`
	}
	get(t, srv, "test-token", "/api/config", &got)
	if got.Raw.PollInterval != "1m" || got.Mtime == "" {
		t.Fatalf("got %+v", got)
	}

	got.Raw.PollInterval = "5m"
	body, _ := json.Marshal(map[string]any{"raw": got.Raw, "base_mtime": got.Mtime})
	req, _ := http.NewRequest("PUT", srv.URL+"/api/config", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("put status = %d", res.StatusCode)
	}
	// 古い mtime での再 PUT は 409
	res2Req, _ := http.NewRequest("PUT", srv.URL+"/api/config", bytes.NewReader(body))
	res2Req.Header.Set("Authorization", "Bearer test-token")
	res2, _ := http.DefaultClient.Do(res2Req)
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d", res2.StatusCode)
	}
}
```

- [ ] **Step 2: 失敗を確認** — `go test ./internal/appserver/ -run 'TestSaveConfig|TestConfigGetPut' -count=1` → FAIL
- [ ] **Step 3: 実装**

`yamledit.go` の核心(完全実装):

```go
func SaveConfig(path string, raw *config.Raw, baseMtime time.Time) error {
	oldBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.ModTime().Equal(baseMtime) {
		return ErrConflict
	}

	// 新ツリーを組み立て、旧ツリーからコメントを移植する
	var oldRoot yaml.Node
	if err := yaml.Unmarshal(oldBytes, &oldRoot); err != nil {
		return fmt.Errorf("parse existing config: %w", err)
	}
	var newRoot yaml.Node
	nb, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(nb, &newRoot); err != nil {
		return err
	}
	if len(oldRoot.Content) > 0 && len(newRoot.Content) > 0 {
		newRoot.HeadComment = oldRoot.HeadComment
		newRoot.Content[0].HeadComment = oldRoot.Content[0].HeadComment
		mergeComments(oldRoot.Content[0], newRoot.Content[0])
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&newRoot); err != nil {
		return err
	}
	_ = enc.Close()

	// 検証が通った場合のみ書き込む(不変条件: 壊れた設定を書かない)
	if _, err := config.Parse(buf.Bytes(), path); err != nil {
		return err
	}

	if err := os.WriteFile(path+".bak", oldBytes, 0o600); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mergeComments は new 側の各ノードに old 側の対応ノードのコメントを移植する。
func mergeComments(oldN, newN *yaml.Node) {
	if oldN == nil || newN == nil {
		return
	}
	copyComments := func(src, dst *yaml.Node) {
		if dst.HeadComment == "" {
			dst.HeadComment = src.HeadComment
		}
		if dst.LineComment == "" {
			dst.LineComment = src.LineComment
		}
		if dst.FootComment == "" {
			dst.FootComment = src.FootComment
		}
	}
	switch newN.Kind {
	case yaml.MappingNode:
		if oldN.Kind != yaml.MappingNode {
			return
		}
		oldVals := map[string][2]*yaml.Node{}
		for i := 0; i+1 < len(oldN.Content); i += 2 {
			oldVals[oldN.Content[i].Value] = [2]*yaml.Node{oldN.Content[i], oldN.Content[i+1]}
		}
		for i := 0; i+1 < len(newN.Content); i += 2 {
			if pair, ok := oldVals[newN.Content[i].Value]; ok {
				copyComments(pair[0], newN.Content[i])
				copyComments(pair[1], newN.Content[i+1])
				mergeComments(pair[1], newN.Content[i+1])
			}
		}
	case yaml.SequenceNode:
		if oldN.Kind != yaml.SequenceNode {
			return
		}
		// 要素の対応付け: マップ要素なら id または (from,to)、それ以外は位置
		key := func(n *yaml.Node) string {
			if n.Kind != yaml.MappingNode {
				return ""
			}
			var id, from, to string
			for i := 0; i+1 < len(n.Content); i += 2 {
				switch n.Content[i].Value {
				case "id":
					id = n.Content[i+1].Value
				case "from":
					from = n.Content[i+1].Value
				case "to":
					to = n.Content[i+1].Value
				}
			}
			if id != "" {
				return "id:" + id
			}
			if from != "" || to != "" {
				return "pair:" + from + "->" + to
			}
			return ""
		}
		oldByKey := map[string]*yaml.Node{}
		for _, c := range oldN.Content {
			if k := key(c); k != "" {
				oldByKey[k] = c
			}
		}
		for i, c := range newN.Content {
			var src *yaml.Node
			if k := key(c); k != "" {
				src = oldByKey[k]
			} else if i < len(oldN.Content) {
				src = oldN.Content[i]
			}
			if src != nil {
				copyComments(src, c)
				mergeComments(src, c)
			}
		}
	}
}
```

`confighttp.go`:

```go
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	var raw config.Raw
	if err := yaml.Unmarshal(b, &raw); err != nil {
		writeErr(w, http.StatusInternalServerError, "config_parse", err.Error(), "設定ファイルの YAML が壊れています。手で修復してください")
		return
	}
	fi, _ := os.Stat(s.ConfigPath)
	writeJSON(w, map[string]any{"raw": raw, "mtime": strconv.FormatInt(fi.ModTime().UnixNano(), 10)})
}

func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Raw       config.Raw `json:"raw"`
		BaseMtime string     `json:"base_mtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), "")
		return
	}
	nanos, err := strconv.ParseInt(body.BaseMtime, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_mtime", "base_mtime must be a unixnano string", "")
		return
	}
	if err := SaveConfig(s.ConfigPath, &body.Raw, time.Unix(0, nanos)); err != nil {
		switch {
		case errors.Is(err, ErrConflict):
			writeErr(w, http.StatusConflict, "conflict", "config file changed on disk", "外部で変更されています。再読み込みしてください")
		default:
			writeErr(w, http.StatusBadRequest, "invalid_config", err.Error(), "")
		}
		return
	}
	fi, _ := os.Stat(s.ConfigPath)
	writeJSON(w, map[string]any{"ok": true, "mtime": strconv.FormatInt(fi.ModTime().UnixNano(), 10)})
}
```

`Handler()` に `GET /api/config` / `PUT /api/config` を登録。

- [ ] **Step 4: 通過確認** — `go test ./internal/appserver/ -race -count=1` → PASS
- [ ] **Step 5: vet/gofmt 確認してコミット** — `feat(appserver): add config read and comment-preserving write endpoints`

---

### Task 8: OAuth 認可エンドポイント

`POST /api/auth/start` / `GET /api/auth/state` / `POST /api/auth/cancel`。フロー本体は注入可能にしてテストする。

**Files:**
- Create: `internal/appserver/authflow.go`
- Test: `internal/appserver/authflow_test.go`

**Interfaces:**
- Produces: `Server.RunFlow func(ctx context.Context, ocfg *oauth2.Config, port int) (*oauth2.Token, error)`(`New` で既定 `auth.RunLoopbackFlow` を設定)
- `POST /api/auth/start` ボディ `{"account_id":"...","provider":"google|microsoft"}` → 202。既存アカウント id の再認可も許す(トークン上書き)。進行中に再度呼ぶと 409 `{"code":"auth_in_progress"}`
- `GET /api/auth/state` → `{"phase":"idle|running|done|error","account_id":"...","error":"...","hint":"..."}`
- `POST /api/auth/cancel` → 実行中フローの context をキャンセルして `{"ok":true}`
- 成功時は `auth.TokenStore{Dir: DataDir}.Save(accountID, tok)` まで行う(YAML には触らない — 追記はフロント側が PUT /api/config で行う。仕様 §8 の順序)

- [ ] **Step 1: 失敗するテストを書く** — `authflow_test.go`:

```go
func TestAuthFlowLifecycle(t *testing.T) {
	s, dir := testServer(t)
	release := make(chan struct{})
	s.RunFlow = func(ctx context.Context, ocfg *oauth2.Config, port int) (*oauth2.Token, error) {
		select {
		case <-release:
			return &oauth2.Token{AccessToken: "at", RefreshToken: "rt"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func(path, body string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := post("/api/auth/start", `{"account_id":"new-acct","provider":"microsoft"}`); res.StatusCode != http.StatusAccepted {
		t.Fatalf("start = %d", res.StatusCode)
	}
	// 進行中の二重開始は 409
	if res := post("/api/auth/start", `{"account_id":"other","provider":"microsoft"}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("second start = %d", res.StatusCode)
	}
	close(release)
	// done になるまでポーリング
	deadline := time.Now().Add(2 * time.Second)
	for {
		var st struct{ Phase string `json:"phase"` }
		get(t, srv, "test-token", "/api/auth/state", &st)
		if st.Phase == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %s", st.Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// トークンが保存されている
	if _, err := os.Stat(filepath.Join(dir, "tokens", "new-acct.json")); err != nil {
		t.Fatalf("token not saved: %v", err)
	}
}

func TestAuthFlowCancel(t *testing.T) {
	s, _ := testServer(t)
	s.RunFlow = func(ctx context.Context, ocfg *oauth2.Config, port int) (*oauth2.Token, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/auth/start", strings.NewReader(`{"account_id":"x","provider":"microsoft"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(req)
	creq, _ := http.NewRequest("POST", srv.URL+"/api/auth/cancel", nil)
	creq.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(creq)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var st struct{ Phase string `json:"phase"` }
		get(t, srv, "test-token", "/api/auth/state", &st)
		if st.Phase == "error" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %s", st.Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

備考: microsoft はクレデンシャルファイル不要(`client_id` のみ)なので、`testServer` の seed 設定に `microsoft: {client_id: test-client-id}` を追加しておくこと(Task 5 の seed を修正してよい)。

- [ ] **Step 2: 失敗を確認** — `go test ./internal/appserver/ -run TestAuthFlow -count=1` → FAIL
- [ ] **Step 3: 実装** — `authflow.go`:

```go
type authState struct {
	mu        sync.Mutex
	phase     string // "idle" | "running" | "done" | "error"
	accountID string
	errMsg    string
	hint      string
	cancel    context.CancelFunc
}

func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID string `json:"account_id"`
		Provider  string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), "")
		return
	}
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	acct := config.Account{ID: body.AccountID, Provider: body.Provider}
	ocfg, err := clients.OAuthConfigFor(cfg, acct)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "oauth_config", err.Error(),
			"providers 設定(credentials_file / client_id)を確認してください")
		return
	}

	s.authSt.mu.Lock()
	if s.authSt.phase == "running" {
		s.authSt.mu.Unlock()
		writeErr(w, http.StatusConflict, "auth_in_progress", "another authorization is running", "")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	s.authSt.phase, s.authSt.accountID, s.authSt.errMsg, s.authSt.cancel = "running", body.AccountID, "", cancel
	s.authSt.mu.Unlock()

	go func() {
		defer cancel()
		tok, err := s.RunFlow(ctx, ocfg, 0)
		s.authSt.mu.Lock()
		defer s.authSt.mu.Unlock()
		if err != nil {
			s.authSt.phase = "error"
			s.authSt.errMsg = err.Error()
			s.authSt.hint = "ブラウザでの認可が完了しませんでした。再試行してください"
			return
		}
		tokens := &auth.TokenStore{Dir: s.DataDir}
		if err := tokens.Save(body.AccountID, tok); err != nil {
			s.authSt.phase = "error"
			s.authSt.errMsg = err.Error()
			return
		}
		s.authSt.phase = "done"
	}()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"ok": true})
}
```

(`Server` に `authSt authState` と `RunFlow` フィールドを追加。`New` で `RunFlow: auth.RunLoopbackFlow`、`authSt.phase = "idle"`。`handleAuthState` はスナップショットを JSON で返す。`handleAuthCancel` は `cancel` があれば呼ぶ。ルート登録 3 本)

- [ ] **Step 4: 通過確認** — `go test ./internal/appserver/ -race -count=1` → PASS(-race で authState の競合がないこと)
- [ ] **Step 5: vet/gofmt 確認してコミット** — `feat(appserver): add oauth authorization endpoints`

---

### Task 9: カレンダーリスト・doctor エンドポイント

`GET /api/accounts/{id}/calendars?provider=google` と `GET /api/doctor`。

**Files:**
- Create: `internal/appserver/calendars.go`・`internal/appserver/doctorhttp.go`
- Test: `internal/appserver/calendars_test.go`

**Interfaces:**
- Produces: `Server.ListCals func(ctx context.Context, cfg *config.Config, acct config.Account, dataDir string) ([]google.CalendarListEntry, error)`(既定実装は `clients.BuildGoogleClient` → `ListCalendars`)
- `GET /api/accounts/{id}/calendars?provider=google` → `{"calendars":[...]}`。id は設定に未登録でも可(追加ウィザードで使うため)。provider が google 以外 → 400 `{"code":"unsupported_provider"}`。トークンなし → 409 `{"code":"token_missing","hint":"先に認可を実行してください"}`。API エラー → 502 `{"code":"provider_error","hint":"スコープ不足の場合は再認可すると取得できます。カレンダー ID の手入力でも設定できます"}`
- `GET /api/doctor` → `{"ok":bool,"text":"<doctor.Run の出力>"}`(probe は `clients.BuildProvider` + `GetCalendarTimezone`、`cmd_doctor.go` の probe と同じ実装)

- [ ] **Step 1: 失敗するテストを書く** — `calendars_test.go`:

```go
func TestCalendarsEndpoint(t *testing.T) {
	s, _ := testServer(t)
	s.ListCals = func(ctx context.Context, cfg *config.Config, acct config.Account, dataDir string) ([]google.CalendarListEntry, error) {
		if acct.ID != "new-acct" {
			t.Errorf("acct = %+v", acct)
		}
		return []google.CalendarListEntry{{ID: "primary", Summary: "Main", Primary: true, AccessRole: "owner"}}, nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got struct {
		Calendars []google.CalendarListEntry `json:"calendars"`
	}
	res := get(t, srv, "test-token", "/api/accounts/new-acct/calendars?provider=google", &got)
	if res.StatusCode != 200 || len(got.Calendars) != 1 {
		t.Fatalf("res=%d got=%+v", res.StatusCode, got)
	}
	// microsoft は 400
	res2 := get(t, srv, "test-token", "/api/accounts/work-ms/calendars?provider=microsoft", nil)
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("ms status = %d", res2.StatusCode)
	}
}
```

- [ ] **Step 2: 失敗を確認** — `go test ./internal/appserver/ -run TestCalendarsEndpoint -count=1` → FAIL
- [ ] **Step 3: 実装** — `calendars.go`(既定 `ListCals` は `New` で配線):

```go
func defaultListCals(ctx context.Context, cfg *config.Config, acct config.Account, dataDir string) ([]google.CalendarListEntry, error) {
	tokens := &auth.TokenStore{Dir: dataDir}
	c, err := clients.BuildGoogleClient(cfg, tokens, acct)
	if err != nil {
		return nil, err
	}
	return c.ListCalendars(ctx)
}

func (s *Server) handleCalendars(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("provider") != "google" {
		writeErr(w, http.StatusBadRequest, "unsupported_provider",
			"calendar listing is only available for google accounts",
			"microsoft は v1 では primary 固定です")
		return
	}
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	acct := config.Account{ID: r.PathValue("id"), Provider: "google"}
	tokens := &auth.TokenStore{Dir: s.DataDir}
	if _, err := tokens.Load(acct.ID); err != nil {
		writeErr(w, http.StatusConflict, "token_missing", err.Error(), "先に認可を実行してください")
		return
	}
	cals, err := s.ListCals(r.Context(), cfg, acct, s.DataDir)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "provider_error", err.Error(),
			"スコープ不足の場合は再認可すると取得できます。カレンダー ID の手入力でも設定できます")
		return
	}
	writeJSON(w, map[string]any{"calendars": cals})
}
```

`doctorhttp.go`:

```go
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	tokens := &auth.TokenStore{Dir: s.DataDir}
	probe := s.Probe
	if probe == nil {
		probe = func(ctx context.Context, acct config.Account) error {
			p, err := clients.BuildProvider(cfg, tokens, acct)
			if err != nil {
				return err
			}
			cal := "primary"
			if len(acct.Calendars) > 0 {
				cal = acct.Calendars[0]
			}
			_, err = p.GetCalendarTimezone(ctx, model.CalendarRef{AccountID: acct.ID, CalendarID: cal})
			return err
		}
	}
	var buf bytes.Buffer
	runErr := doctor.Run(r.Context(), cfg, s.DataDir, probe, &buf, s.ConfigPath)
	writeJSON(w, map[string]any{"ok": runErr == nil, "text": buf.String()})
}
```

(`Server` に `Probe doctor.Probe` フィールド追加。doctor の DB アクセスも OpenReadOnly のみなので不変条件は保たれるが、**launchd 検出時以外は doctor も DB を読まないよう** `handleDoctor` 冒頭で `s.detectDaemon` を確認し、mode が launchd 以外なら DB 部分をスキップさせる…は doctor.Run 内部の分岐になるため、v1 は「mode != launchd のときは 409 `{"code":"not_launchd"}` で拒否」とする。テスト側では plist を作って launchd モードにしてから叩く)

- [ ] **Step 4: 通過確認** — `go test ./internal/appserver/ -race -count=1` → PASS
- [ ] **Step 5: vet/gofmt 確認してコミット** — `feat(appserver): add calendar listing and doctor endpoints`

---

### Task 10: cmd_appserver.go(CLI 配線)

`calsync appserver` サブコマンドを追加する。

**Files:**
- Create: `cmd/calsync/cmd_appserver.go`
- Test: `cmd/calsync/cli_test.go`(追記)

**Interfaces:**
- Consumes: `appserver.New` / `Serve` / `WatchStdinEOF` / `GenerateToken`
- `calsync appserver --config <path> --data <dir>`: 127.0.0.1 のエフェメラルポートで起動し、stdout に `{"port","token"}` を 1 行出力。stdin EOF・SIGINT・SIGTERM で graceful shutdown

- [ ] **Step 1: 失敗するテストを書く** — `cli_test.go` に追記(コマンド登録の確認のみ。Serve 自体は appserver 側でテスト済み):

```go
func TestAppserverCommandRegistered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"appserver"})
	if err != nil || cmd.Name() != "appserver" {
		t.Fatalf("appserver command not registered: %v", err)
	}
}
```

- [ ] **Step 2: 失敗を確認** — `go test ./cmd/calsync/ -run TestAppserverCommandRegistered -count=1` → FAIL
- [ ] **Step 3: 実装** — `cmd_appserver.go`:

```go
package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/btajp/calsync/internal/appserver"
)

var appserverCmd = &cobra.Command{
	Use:   "appserver",
	Short: "Run the localhost API server for the calsync desktop app (spawned as a Tauri sidecar)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		token, err := appserver.GenerateToken()
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		// 親(Tauri 殻)が死んだら stdin が EOF になり自動終了する(孤児化防止)
		appserver.WatchStdinEOF(cmd.InOrStdin(), cancel)
		s := appserver.New(flagConfig, flagData, token)
		return s.Serve(ctx, ln, cmd.OutOrStdout())
	},
}

func init() { rootCmd.AddCommand(appserverCmd) }
```

- [ ] **Step 4: 通過確認** — `go test ./... -race -count=1` → 全 PASS。手動スモーク: `go build -o ./calsync ./cmd/calsync && (echo | ./calsync appserver --config data/calsync.yaml --data data)` が 1 行 JSON を出して即終了すること(stdin EOF)
- [ ] **Step 5: vet/gofmt 確認してコミット** — `feat(cli): add appserver subcommand for the desktop app`

---

### Task 11: desktop/ スキャフォールド(Tauri + React + Vite + sidecar 接続)

Tauri プロジェクトを手書きスキャフォールドで作成し、サイドカー起動〜ハンドシェイク〜API クライアントまでを通す。

**Files:**
- Create: `desktop/package.json`・`desktop/vite.config.ts`・`desktop/tsconfig.json`・`desktop/index.html`
- Create: `desktop/src/main.tsx`・`desktop/src/App.tsx`・`desktop/src/sidecar.ts`・`desktop/src/api.ts`・`desktop/src/types.ts`・`desktop/src/styles.css`
- Create: `desktop/src-tauri/Cargo.toml`・`desktop/src-tauri/tauri.conf.json`・`desktop/src-tauri/build.rs`・`desktop/src-tauri/src/main.rs`・`desktop/src-tauri/src/lib.rs`・`desktop/src-tauri/capabilities/default.json`・`desktop/src-tauri/icons/`(`npx tauri icon` 生成)
- Create: `desktop/scripts/build-sidecar.sh`
- Test: `desktop/src/api.test.ts`(vitest)
- Modify: `.gitignore`(`desktop/node_modules/`・`desktop/dist/`・`desktop/src-tauri/target/`・`desktop/src-tauri/binaries/` を追加)

**Interfaces:**
- Produces(`types.ts`): `config.Raw` の TS ミラー(`RawConfig`・`RawAccount`・`RawDetailSync`・`RawSlack` — フィールド名は Go の json タグと同一の snake_case)、`StatusResponse`・`DaemonInfo`・`TokenStatus`・`CalendarStatus`・`CalendarListEntry`・`AuthState`・`ApiError`
- Produces(`api.ts`): `class ApiClient { constructor(baseUrl: string, token: string, fetchFn?: typeof fetch) }` + メソッド `status()` / `getConfig()` / `putConfig(raw, baseMtime)` / `listCalendars(id)` / `authStart(accountId, provider)` / `authState()` / `authCancel()` / `daemon(action)` / `doctor()`。非 2xx は `ApiError`(code/message/hint)を throw
- Produces(`sidecar.ts`): `startSidecar(dataDir: string): Promise<{ api: ApiClient; kill: () => void }>` — `Command.sidecar('binaries/calsync', ['appserver', '--config', `${dataDir}/calsync.yaml`, '--data', dataDir])` を spawn し、stdout 1 行目の `parseHandshake` で ApiClient を作る。`parseHandshake(line: string): {port: number, token: string}` は export してテスト対象にする

- [ ] **Step 1: package.json と設定ファイル群を作成**

`desktop/package.json`:

```json
{
  "name": "calsync-desktop",
  "private": true,
  "version": "0.1.0",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "tsc --noEmit && vite build",
    "typecheck": "tsc --noEmit",
    "test": "vitest run",
    "tauri": "tauri",
    "build-sidecar": "bash scripts/build-sidecar.sh"
  },
  "dependencies": {
    "@tauri-apps/api": "^2",
    "@tauri-apps/plugin-dialog": "^2",
    "@tauri-apps/plugin-shell": "^2",
    "react": "^18",
    "react-dom": "^18"
  },
  "devDependencies": {
    "@tauri-apps/cli": "^2",
    "@types/react": "^18",
    "@types/react-dom": "^18",
    "@vitejs/plugin-react": "^4",
    "typescript": "^5",
    "vite": "^6",
    "vitest": "^3"
  }
}
```

`desktop/vite.config.ts`:

```ts
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  clearScreen: false,
  server: { port: 1420, strictPort: true },
});
```

`desktop/tsconfig.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["ES2022", "DOM"],
    "module": "ESNext",
    "moduleResolution": "bundler",
    "jsx": "react-jsx",
    "strict": true,
    "noEmit": true,
    "skipLibCheck": true
  },
  "include": ["src"]
}
```

`desktop/scripts/build-sidecar.sh`:

```bash
#!/usr/bin/env bash
# Go 本体をビルドして Tauri の externalBin 命名(ターゲットトリプル付き)で配置する
set -euo pipefail
cd "$(dirname "$0")/../.."
TRIPLE=$(rustc -vV | awk '/^host:/{print $2}')
mkdir -p desktop/src-tauri/binaries
go build -o "desktop/src-tauri/binaries/calsync-${TRIPLE}" ./cmd/calsync
echo "sidecar: desktop/src-tauri/binaries/calsync-${TRIPLE}"
```

`desktop/src-tauri/tauri.conf.json`:

```json
{
  "$schema": "https://schema.tauri.app/config/2",
  "productName": "calsync",
  "version": "0.1.0",
  "identifier": "com.btajp.calsync.desktop",
  "build": {
    "beforeDevCommand": "npm run dev",
    "devUrl": "http://localhost:1420",
    "beforeBuildCommand": "npm run build",
    "frontendDist": "../dist"
  },
  "app": {
    "windows": [{ "title": "calsync", "width": 980, "height": 700 }],
    "security": { "csp": null }
  },
  "bundle": {
    "active": true,
    "targets": ["app"],
    "externalBin": ["binaries/calsync"],
    "icon": ["icons/icon.icns"]
  }
}
```

`desktop/src-tauri/Cargo.toml`:

```toml
[package]
name = "calsync-desktop"
version = "0.1.0"
edition = "2021"

[build-dependencies]
tauri-build = { version = "2", features = [] }

[dependencies]
tauri = { version = "2", features = [] }
tauri-plugin-shell = "2"
tauri-plugin-dialog = "2"

[lib]
name = "calsync_desktop_lib"
crate-type = ["staticlib", "cdylib", "rlib"]
```

`desktop/src-tauri/build.rs`:

```rust
fn main() {
    tauri_build::build()
}
```

`desktop/src-tauri/src/main.rs`:

```rust
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    calsync_desktop_lib::run();
}
```

`desktop/src-tauri/src/lib.rs`(殻は薄く: プラグイン登録のみ):

```rust
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_dialog::init())
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
```

`desktop/src-tauri/capabilities/default.json`(最小権限: 自前サイドカーの spawn とフォルダ選択のみ):

```json
{
  "$schema": "../gen/schemas/desktop-schema.json",
  "identifier": "default",
  "windows": ["main"],
  "permissions": [
    "core:default",
    "dialog:allow-open",
    {
      "identifier": "shell:allow-spawn",
      "allow": [{ "name": "binaries/calsync", "sidecar": true, "args": true }]
    },
    "shell:allow-kill",
    "shell:allow-stdin-write"
  ]
}
```

- [ ] **Step 2: 失敗するテストを書く** — `desktop/src/api.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import { ApiClient } from "./api";
import { parseHandshake } from "./sidecar";

describe("parseHandshake", () => {
  it("parses the sidecar handshake line", () => {
    expect(parseHandshake('{"port":12345,"token":"abc"}')).toEqual({ port: 12345, token: "abc" });
  });
  it("throws on garbage", () => {
    expect(() => parseHandshake("boot noise")).toThrow();
  });
});

describe("ApiClient", () => {
  it("sends bearer token and parses errors", async () => {
    const calls: { url: string; init: RequestInit }[] = [];
    const fakeFetch = (async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return new Response(JSON.stringify({ code: "conflict", message: "changed", hint: "reload" }), {
        status: 409,
      });
    }) as unknown as typeof fetch;
    const api = new ApiClient("http://127.0.0.1:1", "tok", fakeFetch);
    await expect(api.getConfig()).rejects.toMatchObject({ code: "conflict" });
    expect(calls[0].url).toBe("http://127.0.0.1:1/api/config");
    expect((calls[0].init.headers as Record<string, string>)["Authorization"]).toBe("Bearer tok");
  });
});
```

- [ ] **Step 3: 失敗を確認** — Run: `cd desktop && npm install && npm test`。Expected: FAIL(api.ts / sidecar.ts 未作成)
- [ ] **Step 4: 実装**

`desktop/src/types.ts`(Go の json タグと同名。全フィールド optional 扱いで安全側に):

```ts
export interface RawSlack {
  bot_token_env?: string;
  channel?: string;
  morning_digest?: string;
  remind_before?: string;
}
export interface RawAccount {
  id?: string;
  provider?: string;
  email?: string;
  calendars?: string[];
  digest_calendars?: string[];
  blocker_calendar?: string;
  show_origin_in_description?: boolean;
}
export interface RawDetailSync {
  from?: string;
  to?: string;
  fields?: string[];
  visibility?: string;
}
export interface RawConfig {
  poll_interval?: string;
  sync_window?: string;
  blocker_title?: string;
  reconcile_at?: string;
  dedupe_same_meeting?: boolean;
  busy_show_as?: string[];
  notifications?: { slack?: RawSlack };
  providers?: {
    google?: { credentials_file?: string };
    microsoft?: { client_id?: string };
  };
  accounts?: RawAccount[];
  detail_sync?: RawDetailSync[];
}
export interface DaemonInfo { mode: string; running: boolean; detail?: string }
export interface TokenStatus { account_id: string; state: string }
export interface CalendarStatus { account_id: string; calendar_id: string; last_sync: string; status: string }
export interface StatusResponse {
  daemon: DaemonInfo;
  tokens: TokenStatus[];
  calendars?: CalendarStatus[];
  db_note?: string;
}
export interface CalendarListEntry { id: string; summary: string; primary: boolean; access_role: string }
export interface AuthState { phase: "idle" | "running" | "done" | "error"; account_id?: string; error?: string; hint?: string }
```

`desktop/src/api.ts`:

```ts
import type { AuthState, CalendarListEntry, RawConfig, StatusResponse } from "./types";

export class ApiError extends Error {
  constructor(
    public code: string,
    message: string,
    public hint?: string,
    public status?: number,
  ) {
    super(message);
  }
}

export class ApiClient {
  constructor(
    private baseUrl: string,
    private token: string,
    private fetchFn: typeof fetch = fetch,
  ) {}

  private async req<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await this.fetchFn(`${this.baseUrl}${path}`, {
      method,
      headers: {
        Authorization: `Bearer ${this.token}`,
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
    const text = await res.text();
    const data = text ? JSON.parse(text) : {};
    if (!res.ok) {
      throw new ApiError(data.code ?? "unknown", data.message ?? res.statusText, data.hint, res.status);
    }
    return data as T;
  }

  status() { return this.req<StatusResponse>("GET", "/api/status"); }
  getConfig() { return this.req<{ raw: RawConfig; mtime: string }>("GET", "/api/config"); }
  putConfig(raw: RawConfig, baseMtime: string) {
    return this.req<{ ok: boolean; mtime: string }>("PUT", "/api/config", { raw, base_mtime: baseMtime });
  }
  listCalendars(id: string) {
    return this.req<{ calendars: CalendarListEntry[] }>("GET", `/api/accounts/${encodeURIComponent(id)}/calendars?provider=google`);
  }
  authStart(accountId: string, provider: string) {
    return this.req<{ ok: boolean }>("POST", "/api/auth/start", { account_id: accountId, provider });
  }
  authState() { return this.req<AuthState>("GET", "/api/auth/state"); }
  authCancel() { return this.req<{ ok: boolean }>("POST", "/api/auth/cancel"); }
  daemon(action: "start" | "stop" | "restart") {
    return this.req<{ ok: boolean }>("POST", `/api/daemon/${action}`);
  }
  doctor() { return this.req<{ ok: boolean; text: string }>("GET", "/api/doctor"); }
}
```

`desktop/src/sidecar.ts`:

```ts
import { Command } from "@tauri-apps/plugin-shell";
import { ApiClient } from "./api";

export function parseHandshake(line: string): { port: number; token: string } {
  const parsed = JSON.parse(line) as { port?: number; token?: string };
  if (typeof parsed.port !== "number" || typeof parsed.token !== "string") {
    throw new Error(`invalid handshake: ${line}`);
  }
  return { port: parsed.port, token: parsed.token };
}

export async function startSidecar(dataDir: string): Promise<{ api: ApiClient; kill: () => void }> {
  const cmd = Command.sidecar("binaries/calsync", [
    "appserver",
    "--config", `${dataDir}/calsync.yaml`,
    "--data", dataDir,
  ]);
  // spawn は 1 回だけ。stdout リスナは spawn 前に登録し、handshake 行が来たら resolve する
  return new Promise((resolve, reject) => {
    let child: Awaited<ReturnType<typeof cmd.spawn>> | undefined;
    const timer = setTimeout(() => {
      void child?.kill();
      reject(new Error("sidecar handshake timeout"));
    }, 10_000);
    cmd.stdout.on("data", (line: string) => {
      try {
        const hs = parseHandshake(line.trim());
        clearTimeout(timer);
        resolve({
          api: new ApiClient(`http://127.0.0.1:${hs.port}`, hs.token),
          kill: () => { void child?.kill(); },
        });
      } catch {
        // 起動ノイズ行は無視して次の行を待つ
      }
    });
    cmd.on("error", (e: string) => { clearTimeout(timer); reject(new Error(e)); });
    void cmd.spawn().then((c) => { child = c; }).catch((e) => { clearTimeout(timer); reject(e); });
  });
}
```

`desktop/src/App.tsx`(この時点では接続確認まで。ページは Task 12-14 で追加):

```tsx
import { useEffect, useState } from "react";
import { open } from "@tauri-apps/plugin-dialog";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { ApiClient } from "./api";
import { startSidecar } from "./sidecar";

export default function App() {
  const [api, setApi] = useState<ApiClient | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [dataDir, setDataDir] = useState<string | null>(localStorage.getItem("calsync.dataDir"));

  useEffect(() => {
    if (!dataDir) return;
    let kill: (() => void) | undefined;
    startSidecar(dataDir)
      .then((s) => {
        setApi(s.api);
        kill = s.kill;
        void getCurrentWindow().onCloseRequested(() => s.kill());
      })
      .catch((e) => setError(String(e)));
    return () => kill?.();
  }, [dataDir]);

  if (!dataDir) {
    return (
      <main className="setup">
        <h1>calsync</h1>
        <p>calsync のデータディレクトリ(calsync.yaml があるフォルダ)を選択してください。</p>
        <button
          onClick={async () => {
            const dir = await open({ directory: true });
            if (typeof dir === "string") {
              localStorage.setItem("calsync.dataDir", dir);
              setDataDir(dir);
            }
          }}
        >
          フォルダを選択
        </button>
      </main>
    );
  }
  if (error) return <main className="error">起動エラー: {error}</main>;
  if (!api) return <main>appserver に接続中…</main>;
  return <Shell api={api} onResetDataDir={() => { localStorage.removeItem("calsync.dataDir"); setDataDir(null); }} />;
}

function Shell({ api }: { api: ApiClient; onResetDataDir: () => void }) {
  const [ping, setPing] = useState<string>("...");
  useEffect(() => {
    api.status().then((s) => setPing(`daemon: ${s.daemon.mode}`)).catch((e) => setPing(String(e)));
  }, [api]);
  return <main><h1>calsync</h1><p>{ping}</p></main>;
}
```

`desktop/src/main.tsx`・`desktop/index.html`・`desktop/src/styles.css` は通常の Vite + React 最小構成(styles.css はシステムフォント・余白・テーブル罫線程度)。

- [ ] **Step 5: テスト・型チェック通過を確認** — `cd desktop && npm test && npm run typecheck` → PASS
- [ ] **Step 6: アイコン生成と手動スモーク** — `cd desktop && npx tauri icon`(既定アイコンで良い。要 `src-tauri/icons/`)→ `npm run build-sidecar && npm run tauri dev` でウィンドウが開き、フォルダ選択 → `daemon: ...` が表示されることを確認。ウィンドウを閉じて `pgrep -f "calsync appserver"` が空(孤児なし)を確認
- [ ] **Step 7: .gitignore 追記を含めてコミット** — `feat(desktop): scaffold Tauri app with sidecar handshake`

---

### Task 12: ダッシュボード+構成俯瞰画面

status/config/doctor を合成した一覧画面と、デーモン制御ボタン。

**Files:**
- Create: `desktop/src/pages/Dashboard.tsx`
- Modify: `desktop/src/App.tsx`(タブナビゲーション: ダッシュボード / 設定 / アカウント追加)
- Test: `desktop/src/pages/dashboard.test.ts`(整形ロジックのみ)

**Interfaces:**
- Consumes: `ApiClient.status()` / `daemon()` / `doctor()` / `getConfig()`
- Produces: `buildOverview(raw: RawConfig, status: StatusResponse): OverviewRow[]` を `Dashboard.tsx` から export(`OverviewRow = { accountId: string; provider: string; watched: string[]; blocker: string; digest: string[]; tokenState: string; lastSync: string; syncStatus: string }`)。カレンダー単位の最終同期は `status.calendars` から accountId で突合し、最も新しいものを表示

- [ ] **Step 1: 失敗するテストを書く** — `dashboard.test.ts`: `buildOverview` に「google 2 カレンダー監視+digest 1 件」「token missing の microsoft」を与え、watched/blocker(既定 primary)/tokenState が期待通りになることを assert(具体値: accounts `personal`(calendars `["primary","xxxxx@group.calendar.google.com"]`)と `work-ms`。tokens は `[{account_id:"personal",state:"ok"},{account_id:"work-ms",state:"missing"}]`)
- [ ] **Step 2: 失敗を確認** — `npm test` → FAIL
- [ ] **Step 3: 実装** — `Dashboard.tsx`: `buildOverview`(純関数)+ 画面。画面要素: (1) デーモン状態カード(mode/running、mode=launchd なら 停止/起動/再起動ボタン → `api.daemon()`、実行後 `status()` 再取得。mode=container なら「コンテナ運用中のためホストからの操作・読み取りはできません」の案内。manual なら install-launchd.sh の案内)(2) 俯瞰テーブル(OverviewRow)(3) doctor 実行ボタン → `<pre>{text}</pre>` 表示。5 秒ポーリングは `useEffect` + `setInterval`(`document.visibilityState === "visible"` のときのみ)
- [ ] **Step 4: 通過確認** — `npm test && npm run typecheck` → PASS。`npm run tauri dev` で実データ表示・再起動ボタン動作を目視確認
- [ ] **Step 5: コミット** — `feat(desktop): add dashboard with daemon control and account overview`

---

### Task 13: 設定フォーム画面

**Files:**
- Create: `desktop/src/pages/ConfigForm.tsx`
- Modify: `desktop/src/App.tsx`(タブ登録)
- Test: `desktop/src/pages/configform.test.ts`

**Interfaces:**
- Consumes: `getConfig()` / `putConfig()`
- Produces: `normalizeRaw(raw: RawConfig): RawConfig` を export — フォーム値→送信ボディの整形(空文字のフィールドを undefined に落とす、busy_show_as の空配列を undefined に、accounts の空 calendars を undefined に)。**「フォームが値を持たない = キーを出力しない」を徹底し、YAML に空キーを撒かない**

- [ ] **Step 1: 失敗するテストを書く** — `configform.test.ts`: `normalizeRaw({ poll_interval: "", accounts: [{ id: "personal", provider: "google", calendars: [] }] })` → `poll_interval` が undefined・`calendars` が undefined になること。値があるものは素通しすること
- [ ] **Step 2: 失敗を確認** — `npm test` → FAIL
- [ ] **Step 3: 実装** — `ConfigForm.tsx`:
  - グローバル設定セクション: poll_interval / sync_window / blocker_title / reconcile_at(text)、dedupe_same_meeting(checkbox・未指定と false を区別するため「既定(true)/true/false」の 3 択 select)、busy_show_as(チェックボックス群: free/tentative/busy/oof/workingElsewhere/unknown)
  - Slack セクション: bot_token_env / channel / morning_digest / remind_before
  - providers セクション: credentials_file / client_id
  - accounts セクション: 行の追加はここではしない(追加はウィザードへ誘導)。既存行の calendars(カンマ区切り text)・digest_calendars・blocker_calendar・show_origin_in_description・email を編集可
  - detail_sync セクション: from/to(既存アカウント id の select)・fields(title/description チェック)・visibility(select)。行追加・削除可
  - 保存ボタン: `putConfig(normalizeRaw(raw), mtime)` → 409 なら「外部で変更されています」+再読み込みボタン、400 なら message 表示。成功したら新 mtime を保持し「デーモン未反映の変更があります → 再起動して適用」バナー表示(mode=launchd のときのみ再起動ボタン、それ以外は手順案内)
- [ ] **Step 4: 通過確認** — `npm test && npm run typecheck` → PASS。`npm run tauri dev` で編集→保存→YAML のコメントが残っていること(手で開いて確認)→再起動ボタンまで通す
- [ ] **Step 5: コミット** — `feat(desktop): add config form with comment-preserving save`

---

### Task 14: アカウント追加ウィザード

**Files:**
- Create: `desktop/src/pages/AccountAdd.tsx`
- Modify: `desktop/src/App.tsx`(タブ登録)
- Test: `desktop/src/pages/accountadd.test.ts`

**Interfaces:**
- Consumes: `getConfig()` / `putConfig()` / `authStart()` / `authState()` / `authCancel()` / `listCalendars()`
- Produces: `validateNewAccountId(id: string, existing: string[]): string | null` を export(null = OK。空・重複・`[A-Za-z0-9._-]+` 以外はエラーメッセージ文字列を返す — `internal/auth` の validateAccountID と同じ許容集合)

- [ ] **Step 1: 失敗するテストを書く** — `accountadd.test.ts`: `validateNewAccountId("personal", ["personal"])` → 重複エラー、`validateNewAccountId("ok-id.1", [])` → null、`validateNewAccountId("bad/id", [])` → 形式エラー、`validateNewAccountId("", [])` → 必須エラー
- [ ] **Step 2: 失敗を確認** — `npm test` → FAIL
- [ ] **Step 3: 実装** — `AccountAdd.tsx`(仕様 §8 の順序をステッパーで実装):
  1. **前提チェック**: `getConfig()` で provider 選択に応じ credentials_file / client_id の有無を確認。未設定なら README / calsync-setup の手順名を出して停止
  2. **入力**: id(`validateNewAccountId` で即時検証)・provider(google/microsoft)・email(表示用ラベル・任意)
  3. **認可**: `authStart(id, provider)` → 1 秒間隔で `authState()` ポーリング。running 中はキャンセルボタン(`authCancel()`)。error なら message+hint 表示して再試行可
  4. **カレンダー選択**(google のみ): `listCalendars(id)` → チェックボックスで監視対象(既定 primary)、select でブロッカー先(既定 primary)。`provider_error` の場合は hint を出しつつカンマ区切りの手入力にフォールバック。microsoft はこのステップをスキップ(primary 固定の説明のみ)
  5. **設定追記**: `getConfig()` を取り直し(最新 mtime)、`accounts` に新規行を push して `putConfig`。409 なら取り直して再試行を促す
  6. **反映**: 「デーモンを再起動して反映」ボタン(mode=launchd)。完了画面でダッシュボードへ誘導
- [ ] **Step 4: 通過確認** — `npm test && npm run typecheck` → PASS。可能なら実アカウントで一連のフロー(認可→選択→追記→再起動)を手動確認(**実環境識別子はコミット物に残さないこと**)
- [ ] **Step 5: コミット** — `feat(desktop): add account-add wizard`

---

### Task 15: ドキュメント更新

**Files:**
- Modify: `README.md`(「デスクトップアプリ(macOS)」セクション追加: 前提 Rust+Node、`cd desktop && npm install && npm run build-sidecar && npm run tauri dev` / `tauri build`、できること一覧、コンテナ運用では使えない旨)
- Modify: `README.md` の CLI コマンド表に `calsync appserver` 行を追加
- Modify: `CHANGELOG.md` の `[Unreleased]` に Added(desktop app / appserver subcommand / google calendarlist scope)を追記
- Modify: `.agents/skills/calsync-setup/SKILL.md`(アカウント追加をアプリで行う場合の分岐を追記: OAuth 認可とカレンダー選択・YAML 追記・再起動までアプリで完結すること、Google の追加スコープ calendarlist.readonly に触れる)
- Modify: `.agents/skills/calsync-uninstall/SKILL.md`(デスクトップアプリの撤去 = `desktop/src-tauri/target` の削除と .app の削除、アプリはアンインストール操作を持たない旨)
- Modify: `docs/superpowers/specs/2026-07-21-desktop-app-design.md` §15(実機確認できたスパイク項目の消し込み)

- [ ] **Step 1: 各ドキュメントを更新**(実環境識別子を書かない。例は `personal` / `work-ms`)
- [ ] **Step 2: 突き合わせ確認** — README の手順を頭から実行し、書いてあるコマンドがそのまま動くこと
- [ ] **Step 3: コミット** — `docs: document desktop app setup and appserver command`

---

### Task 16: 最終検証

- [ ] **Step 1: 全チェック** — Run:

```bash
go build -o ./calsync ./cmd/calsync        # CGO なしで通ること
go test ./... -race -count=1               # 全 PASS
go vet ./... && gofmt -l internal/ cmd/    # 出力なし
docker compose config -q                   # 変更なしでも回帰確認
cd desktop && npm run typecheck && npm test && cd ..
```

- [ ] **Step 2: 手動チェックリスト**(結果を PR 本文に記録):
  - [ ] `npm run tauri dev` → フォルダ選択 → ダッシュボード表示(daemon mode 正しい)
  - [ ] 設定変更 → 保存 → YAML のコメント・キー順が保持されている → 再起動ボタン → 反映
  - [ ] 外部エディタで YAML を書き換えてから保存 → 409 と再読み込み誘導
  - [ ] Cmd+Q → `pgrep -f "calsync appserver"` が空(孤児なし)
  - [ ] launchd 未インストール状態(plist を一時退避)で操作ボタンが案内に変わる
- [ ] **Step 3: スパイク消し込み** — 手動チェックで確認できた項目を仕様書 §15 から消し、未確認項目は残す
- [ ] **Step 4: コミット・ブランチ整理** — 残変更をコミットし、`superpowers:finishing-a-development-branch` の手順で PR 作成へ
