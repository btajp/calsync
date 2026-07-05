# Slack 通知(Issue #10)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 朝のダイジェスト(当日の実予定をライブ取得で通知)と開始前リマインド(イベントキャッシュ+送信記録テーブル)を Slack Bot 経由で送る。

**Architecture:** エンジン統合型。`Engine.Notifier` インターフェース(nil で完全無効)を既存 Run ループに組み込み、Slack 実装は `internal/notify/slack` に隔離。ダイジェストは `Changes(cursor="", 当日ウィンドウ)` のライブ取得(カーソル破棄)、リマインドは events キャッシュ(title 列追加)+ `reminders_sent` テーブルで二重送信防止。

**Tech Stack:** Go 1.25(依存追加なし。Slack は net/http 直叩き)、SQLite(modernc.org/sqlite)、testify。

**Spec:** `docs/superpowers/specs/2026-07-05-slack-notifications-design.md`(以下「スペック」)。実装判断に迷ったらスペックが正。

## Global Constraints

- 検証コマンド(全タスク共通。「テスト実行」は対象パッケージ、コミット前は全体):
  `go build -o ./calsync ./cmd/calsync` / `go test ./... -race -count=1` / `go vet ./...` / `gofmt -l internal/ cmd/`(出力なしが正)
- **`go mod tidy` 禁止**。依存追加もなし
- コミットは Conventional Commits(英語)。コメントは既存コードに合わせ日本語
- `Title` を `model.TimeHash` の入力に**絶対に含めない**(件名変更でブロッカー更新が走ってはならない)
- 通知の失敗で `Run` を落とさない(ログのみ)
- Slack へ送る外部由来文字列(件名)は `& < >` をエスケープ(メンションインジェクション防止)
- 図を書く場合は Mermaid(ASCII アート禁止)

## タスク依存関係

Task 1 → 2 → 3。Task 4・5・6 は Task 1 完了後に並行可。Task 7 は 6 の後。Task 8 は 2・4・6 の後。Task 9 は 2・4・5・6 の後。Task 10 は 4・7・9 の後。Task 11 は 5 の後。Task 12 は最後。

---

### Task 1: model.NormalizedEvent に Title を追加

**Files:**
- Modify: `internal/model/model.go`(NormalizedEvent 構造体、21〜33 行付近)
- Test: `internal/model/model_test.go`(追記)

**Interfaces:**
- Produces: `model.NormalizedEvent.Title string`(後続の全タスクが参照)

- [ ] **Step 1: 失敗するテストを書く**

`internal/model/model_test.go` に追記:

```go
// Title は通知表示専用であり、TimeHash(ブロッカー変更検出)に影響してはならない
// (スペック 4.1。件名変更でブロッカー更新が走ると Graph/Google への無駄な PATCH が出る)。
func TestTimeHashIgnoresTitle(t *testing.T) {
	base := NormalizedEvent{
		StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
	}
	titled := base
	titled.Title = "設計レビュー"
	require.Equal(t, TimeHash(base), TimeHash(titled))

	allday := NormalizedEvent{IsAllDay: true, AllDayStart: "2026-07-10", AllDayEnd: "2026-07-11"}
	alldayTitled := allday
	alldayTitled.Title = "終日イベント"
	require.Equal(t, TimeHash(allday), TimeHash(alldayTitled))
}
```

既存テストの import(`testing` / `time` / `require`)が揃っているか確認し、無ければ足す。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/model/ -race -count=1 -run TestTimeHashIgnoresTitle`
Expected: FAIL(`unknown field Title` のコンパイルエラー)

- [ ] **Step 3: 最小実装**

`internal/model/model.go` の `NormalizedEvent` に、`ICalUID` の直後の行としてフィールドを追加:

```go
type NormalizedEvent struct {
	ID          string // プロバイダのイベントID(opaque。パース禁止)
	ICalUID     string
	Title       string // 件名(Slack 通知の表示専用。TimeHash には含めない — スペック 4.1)
	StartUTC    time.Time
	...(以下既存のまま)
```

`TimeHash` 関数は**変更しない**。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/model/ -race -count=1`
Expected: PASS(既存テスト含む)

- [ ] **Step 5: コミット**

```bash
git add internal/model/
git commit -m "feat: add Title to NormalizedEvent (never part of TimeHash)"
```

---

### Task 2: events テーブルへの title 列追加(初のマイグレーション)

**Files:**
- Modify: `internal/store/store.go`(const schema の events テーブル+ `Open` + 新関数 `migrate`。import に `strings` 追加)
- Modify: `internal/store/events.go`(`UpsertEvent` / `GetEvent`)
- Test: `internal/store/store_test.go` または新規 `internal/store/migrate_test.go`

**Interfaces:**
- Consumes: `model.NormalizedEvent.Title`(Task 1)
- Produces: events テーブルの `title` 列。`GetEvent` が `Title` を復元する

- [ ] **Step 1: 失敗するテストを書く**

新規 `internal/store/migrate_test.go`:

```go
package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
)

// 旧スキーマ(title 列なし)の DB を Open すると冪等 ALTER で title 列が追加される
// (スペック 4.2: 本リポジトリのマイグレーション方針の最初の適用例)。
func TestOpenMigratesEventsTitleColumn(t *testing.T) {
	dir := t.TempDir()

	// 旧スキーマの DB を直接作る(store.Open を経由しない)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "calsync.db"))
	require.NoError(t, err)
	_, err = db.Exec(`
CREATE TABLE events (
  account_id    TEXT NOT NULL,
  calendar_id   TEXT NOT NULL,
  event_id      TEXT NOT NULL,
  ical_uid      TEXT,
  start_utc     INTEGER,
  end_utc       INTEGER,
  is_all_day    INTEGER NOT NULL DEFAULT 0,
  all_day_start TEXT,
  all_day_end   TEXT,
  time_hash     TEXT NOT NULL,
  PRIMARY KEY (account_id, calendar_id, event_id)
);
INSERT INTO events (account_id, calendar_id, event_id, ical_uid, start_utc, end_utc, time_hash)
VALUES ('a', 'primary', 'ev-old', 'old@example.com', 100, 200, 'h');`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	ref := model.CalendarRef{AccountID: "a", CalendarID: "primary"}

	// 1回目の Open: ALTER が走り、旧行は空タイトルで読める
	st, err := Open(dir)
	require.NoError(t, err)
	ev, err := st.GetEvent(ref, "ev-old")
	require.NoError(t, err)
	require.NotNil(t, ev)
	require.Equal(t, "", ev.Title)

	// title 付きで upsert → 復元できる
	require.NoError(t, st.UpsertEvent(ref, model.NormalizedEvent{
		ID:       "ev-new",
		ICalUID:  "new@example.com",
		Title:    "設計レビュー",
		StartUTC: time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
		IsBusy:   true,
	}))
	got, err := st.GetEvent(ref, "ev-new")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "設計レビュー", got.Title)
	require.NoError(t, st.Close())

	// 2回目の Open: duplicate column が無視され、壊れない(冪等性)
	st2, err := Open(dir)
	require.NoError(t, err)
	got2, err := st2.GetEvent(ref, "ev-new")
	require.NoError(t, err)
	require.Equal(t, "設計レビュー", got2.Title)
	require.NoError(t, st2.Close())
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/store/ -race -count=1 -run TestOpenMigratesEventsTitleColumn`
Expected: FAIL(`ev.Title` が常に "" / `no such column: title` 系)

- [ ] **Step 3: 実装**

(a) `internal/store/store.go` の const schema、events テーブルの `time_hash` 行の直後に追加:

```sql
  time_hash     TEXT NOT NULL,
  title         TEXT NOT NULL DEFAULT '',
```

※ PRIMARY KEY 行より前に置く。

(b) 同ファイルに `migrate` を追加し、`Open` の `db.Exec(schema)` 成功直後に呼ぶ(import に `strings` を追加):

```go
// migrate は既存 DB への後方互換の列追加を行う。方針(スペック 4.2):
// 新規 DB は const schema、既存 DB は Open 時の冪等 ALTER(duplicate column のみ無視)。
// schema version 管理は導入しない。
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE events ADD COLUMN title TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("add events.title: %w", err)
		}
	}
	return nil
}
```

`Open` 内:

```go
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		_ = lock.Unlock()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		_ = lock.Unlock()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
```

(c) `internal/store/events.go` の `UpsertEvent`: INSERT 列に `title`、VALUES に `?` を 1 つ、`DO UPDATE SET` に `title = excluded.title` を追加し、引数リストの `model.TimeHash(ev)` の**前**に `ev.Title` を渡す(列順は `time_hash` の前に `title` を置くのではなく、SQL 上は `time_hash` の後ろに `title` を足すのが差分最小):

```go
	_, err := s.db.Exec(`
INSERT INTO events (account_id, calendar_id, event_id, ical_uid, start_utc, end_utc,
                    is_all_day, all_day_start, all_day_end, time_hash, title)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (account_id, calendar_id, event_id) DO UPDATE SET
  ical_uid      = excluded.ical_uid,
  start_utc     = excluded.start_utc,
  end_utc       = excluded.end_utc,
  is_all_day    = excluded.is_all_day,
  all_day_start = excluded.all_day_start,
  all_day_end   = excluded.all_day_end,
  time_hash     = excluded.time_hash,
  title         = excluded.title`,
		ref.AccountID, ref.CalendarID, ev.ID, ev.ICalUID,
		ev.StartUTC.UTC().Unix(), ev.EndUTC.UTC().Unix(),
		boolToInt(ev.IsAllDay), ev.AllDayStart, ev.AllDayEnd,
		model.TimeHash(ev), ev.Title)
```

(d) `GetEvent`: SELECT に `title` を追加し、`sql.NullString` で受けて `ev.Title = title.String` で復元:

```go
	row := s.db.QueryRow(`
SELECT ical_uid, start_utc, end_utc, is_all_day, all_day_start, all_day_end, title
FROM events WHERE account_id = ? AND calendar_id = ? AND event_id = ?`,
		ref.AccountID, ref.CalendarID, eventID)
	var (
		icalUID, adStart, adEnd, title sql.NullString
		startUTC, endUTC               sql.NullInt64
		isAllDay                       int
	)
	err := row.Scan(&icalUID, &startUTC, &endUTC, &isAllDay, &adStart, &adEnd, &title)
```

`NormalizedEvent` 復元部に `Title: title.String,` を追加。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/store/ -race -count=1`
Expected: PASS(既存テスト含む)

- [ ] **Step 5: エンジン全体の回帰確認とコミット**

Run: `go test ./... -race -count=1`
Expected: PASS

```bash
git add internal/store/
git commit -m "feat: persist event title in the events cache (first idempotent ALTER migration)"
```

---

### Task 3: プロバイダの Title 正規化(google / microsoft / fake)

**Files:**
- Modify: `internal/provider/google/changes.go`(`normalizeEvent`)
- Modify: `internal/provider/microsoft/delta.go`(`deltaEvent` 構造体+ `normalizeDeltaEvent`)
- Test: `internal/provider/google/changes_test.go`、`internal/provider/microsoft/delta_test.go`、`internal/provider/fake/fake_test.go`(各追記)

**Interfaces:**
- Consumes: `model.NormalizedEvent.Title`(Task 1)
- Produces: 両プロバイダ+fake の `Changes` が `Title` を返す

- [ ] **Step 1: 失敗するテストを書く**

(a) `internal/provider/google/changes_test.go` — 既存の normalize 系テストのスタイルに合わせ、summary → Title の検証を追加(既存に normalizeEvent の直接テストがあればケース追加、なければ以下を追加):

```go
func TestNormalizeEventTitle(t *testing.T) {
	ev := normalizeEvent(&calendar.Event{
		Id:      "ev1",
		Summary: "設計レビュー",
		Start:   &calendar.EventDateTime{DateTime: "2026-07-10T01:00:00Z"},
		End:     &calendar.EventDateTime{DateTime: "2026-07-10T02:00:00Z"},
	})
	require.Equal(t, "設計レビュー", ev.Title)

	// cancelled は ID 以外を保証しない既存契約のまま(Title も空)
	del := normalizeEvent(&calendar.Event{Id: "ev2", Status: "cancelled", Summary: "x"})
	require.True(t, del.Deleted)
	require.Equal(t, "", del.Title)
}
```

(b) `internal/provider/microsoft/delta_test.go` — 既存 `TestNormalizeDeltaEvent`(159 行付近)のテーブルに subject ケースを追加、または独立テストを追加:

```go
func TestNormalizeDeltaEventTitle(t *testing.T) {
	busy := map[string]bool{"busy": true}
	de := deltaEvent{
		ID:      "ev1",
		Subject: "設計レビュー",
		ShowAs:  "busy",
		Start:   &graphTime{DateTime: "2026-07-10T01:00:00.0000000", TimeZone: "UTC"},
		End:     &graphTime{DateTime: "2026-07-10T02:00:00.0000000", TimeZone: "UTC"},
	}
	got, err := normalizeDeltaEvent(de, busy)
	require.NoError(t, err)
	require.Equal(t, "設計レビュー", got.Title)
}
```

※ `graphTime` のフィールド名は `delta_test.go` 既存ケースの表記に合わせること(`DateTime`/`TimeZone` でない場合は既存に倣う)。

(c) `internal/provider/fake/fake_test.go` — Title が素通しされる契約テスト:

```go
// fake は実プロバイダと同じ契約で Title を保持・返却する(スペック 4.1)。
func TestChangesPreservesTitle(t *testing.T) {
	f := New()
	cal := model.CalendarRef{AccountID: "a", CalendarID: "primary"}
	f.SetFullState(cal, []model.NormalizedEvent{{ID: "ev1", Title: "設計レビュー", IsBusy: true}})
	evs, _, err := f.Changes(context.Background(), cal, "", model.Window{})
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "設計レビュー", evs[0].Title)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/provider/... -race -count=1 -run 'Title'`
Expected: google / microsoft は FAIL(`unknown field Subject` / Title 空)。fake は PASS する(構造体を素通しするため)— fake のテストは契約の回帰防止として残す

- [ ] **Step 3: 実装**

(a) `internal/provider/google/changes.go` の `normalizeEvent`、`ev.ICalUID = item.ICalUID` の直後に:

```go
	ev.Title = item.Summary
```

`fields` によるフィールド絞り込みは現行実装に存在しないため、summary は応答に含まれる(スペック 13 章スパイク 2 の前半はコード確認で解消)。

(b) `internal/provider/microsoft/delta.go` の `deltaEvent` に追加:

```go
	Subject        string               `json:"subject"`
```

`normalizeDeltaEvent` の削除判定(`de.Removed != nil || de.IsCancelled`)の**後**に:

```go
	// NOTE: delta 応答に subject が含まれることはユニット(フィクスチャ)で検証済み。
	// 実 API の応答は初回稼働時に要実測(スペック 13 章スパイク 1)
	ev.Title = de.Subject
```

(c) fake は変更不要(`Changes` が `NormalizedEvent` を素通しするため)。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/provider/... -race -count=1`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/provider/
git commit -m "feat: normalize event titles from Google summary and Graph subject"
```

---

### Task 4: config に notifications セクションを追加

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`(既存 `TestLoad` テーブルにケース追加)

**Interfaces:**
- Produces:
  - `config.Config.Notifications NotificationsConfig`
  - `type NotificationsConfig struct { Slack *SlackConfig }`(Slack が nil = 通知無効)
  - `type SlackConfig struct { BotTokenEnv, Channel, MorningDigest string; RemindBefore time.Duration }`

- [ ] **Step 1: 失敗するテストを書く**

`internal/config/config_test.go` の `TestLoad` テーブルにケースを追加(`minimalYAML` に notifications を足した YAML を各ケースで組み立てる):

```go
		{
			name: "slack notifications parsed with defaults",
			yaml: minimalYAML + `
notifications:
  slack:
    channel: "C0123"
    morning_digest: "07:30"
    remind_before: 10m
`,
			check: func(t *testing.T, c *Config) {
				sc := c.Notifications.Slack
				require.NotNil(t, sc)
				require.Equal(t, "SLACK_BOT_TOKEN", sc.BotTokenEnv)
				require.Equal(t, "C0123", sc.Channel)
				require.Equal(t, "07:30", sc.MorningDigest)
				require.Equal(t, 10*time.Minute, sc.RemindBefore)
			},
		},
		{
			name: "slack custom bot_token_env and digest-only",
			yaml: minimalYAML + `
notifications:
  slack:
    bot_token_env: MY_SLACK_TOKEN
    channel: "U0456"
    morning_digest: "06:00"
`,
			check: func(t *testing.T, c *Config) {
				sc := c.Notifications.Slack
				require.NotNil(t, sc)
				require.Equal(t, "MY_SLACK_TOKEN", sc.BotTokenEnv)
				require.Equal(t, time.Duration(0), sc.RemindBefore)
			},
		},
		{
			name: "no notifications section means disabled",
			yaml: minimalYAML,
			check: func(t *testing.T, c *Config) {
				require.Nil(t, c.Notifications.Slack)
			},
		},
		{
			name:    "slack requires channel",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    morning_digest: \"07:30\"\n",
			wantErr: "notifications.slack.channel is required",
		},
		{
			name:    "slack rejects invalid morning_digest",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n    morning_digest: \"7時半\"\n",
			wantErr: "invalid notifications.slack.morning_digest",
		},
		{
			name:    "slack rejects non-positive remind_before",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n    remind_before: -5m\n",
			wantErr: "invalid notifications.slack.remind_before",
		},
		{
			name:    "slack rejects remind_before shorter than poll_interval",
			yaml:    "poll_interval: 5m\n" + minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n    remind_before: 1m\n",
			wantErr: "must be >= poll_interval",
		},
		{
			name:    "slack requires at least one of digest or remind",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n",
			wantErr: "set at least one of morning_digest or remind_before",
		},
		{
			name:    "unknown notification keys are rejected",
			yaml:    minimalYAML + "\nnotifications:\n  slack:\n    channel: \"C1\"\n    morning_digest: \"07:30\"\n    webhook_url: \"https://x\"\n",
			wantErr: "webhook_url",
		},
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/config/ -race -count=1`
Expected: FAIL(`Notifications` 未定義のコンパイルエラー)

- [ ] **Step 3: 実装**

`internal/config/config.go`:

(a) `Config` に追加(`Providers` の上):

```go
	Notifications     NotificationsConfig
```

(b) 型定義(`ProvidersConfig` の近く):

```go
// NotificationsConfig は通知設定。Slack が nil なら通知機能は完全に無効
// (Engine.Notifier が注入されない。スペック 3 章)。
type NotificationsConfig struct {
	Slack *SlackConfig
}

// SlackConfig は Slack Bot 通知の設定(スペック 3 章)。
type SlackConfig struct {
	BotTokenEnv   string        // トークンを読む環境変数名。既定 "SLACK_BOT_TOKEN"
	Channel       string        // C…/G…: チャンネル / U…: DM(conversations.open で解決)
	MorningDigest string        // "HH:MM"(コンテナ TZ)。空ならダイジェスト無効
	RemindBefore  time.Duration // 0 ならリマインド無効
}
```

(c) raw 構造体(`rawConfig` に `Notifications rawNotifications \`yaml:"notifications"\`` を追加):

```go
type rawNotifications struct {
	Slack *rawSlack `yaml:"slack"`
}

type rawSlack struct {
	BotTokenEnv   string `yaml:"bot_token_env"`
	Channel       string `yaml:"channel"`
	MorningDigest string `yaml:"morning_digest"`
	RemindBefore  string `yaml:"remind_before"`
}
```

(d) `Load` の busy_show_as 検証の後・accounts ループの前に:

```go
	if rs := raw.Notifications.Slack; rs != nil {
		sc := &SlackConfig{BotTokenEnv: rs.BotTokenEnv, Channel: rs.Channel, MorningDigest: rs.MorningDigest}
		if sc.BotTokenEnv == "" {
			sc.BotTokenEnv = "SLACK_BOT_TOKEN"
		}
		if sc.Channel == "" {
			return nil, fmt.Errorf("config: notifications.slack.channel is required")
		}
		if sc.MorningDigest != "" {
			if _, err := time.Parse("15:04", sc.MorningDigest); err != nil {
				return nil, fmt.Errorf("config: invalid notifications.slack.morning_digest %q (want \"HH:MM\", e.g. \"07:30\")", sc.MorningDigest)
			}
		}
		if rs.RemindBefore != "" {
			d, err := time.ParseDuration(rs.RemindBefore)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("config: invalid notifications.slack.remind_before %q (want a positive Go duration such as \"10m\")", rs.RemindBefore)
			}
			// リマインド判定は tick(poll_interval)毎のため、これを下回るとウィンドウが
			// tick 間隔より狭くなり発火保証がなくなる(スペック 3 章)
			if d < cfg.PollInterval {
				return nil, fmt.Errorf("config: notifications.slack.remind_before %q must be >= poll_interval %q (reminders are checked once per poll tick)", rs.RemindBefore, cfg.PollInterval)
			}
			sc.RemindBefore = d
		}
		if sc.MorningDigest == "" && sc.RemindBefore == 0 {
			return nil, fmt.Errorf("config: notifications.slack: set at least one of morning_digest or remind_before")
		}
		cfg.Notifications.Slack = sc
	}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/config/ -race -count=1`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/config/
git commit -m "feat: add notifications.slack config section with validation"
```

---

### Task 5: store に reminders_sent テーブルとクエリを追加

**Files:**
- Modify: `internal/store/store.go`(const schema に reminders_sent を追加)
- Create: `internal/store/reminders.go`
- Test: `internal/store/reminders_test.go`

**Interfaces:**
- Produces(Task 9・11 が使う):
  - `type UpcomingEvent struct { Ref model.CalendarRef; EventID, ICalUID, Title string; StartUTC, EndUTC time.Time }`
  - `func (s *Store) ListUpcomingEvents(now time.Time, lead time.Duration) ([]UpcomingEvent, error)`
  - `func (s *Store) WasReminderSent(ref model.CalendarRef, eventID string, startUTC time.Time) (bool, error)`
  - `func (s *Store) HasReminderForICalUID(icalUID string, startUTC time.Time) (bool, error)`
  - `func (s *Store) MarkReminderSent(ref model.CalendarRef, eventID, icalUID string, startUTC, sentAt time.Time) error`
  - `func (s *Store) CleanupRemindersSent(before time.Time) error`
  - `func (s *Store) DeleteRemindersForAccount(accountID string) error`

- [ ] **Step 1: 失敗するテストを書く**

新規 `internal/store/reminders_test.go`:

```go
package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
)

func remTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st
}

var remRef = model.CalendarRef{AccountID: "a", CalendarID: "primary"}

func remEvent(id, ical, title string, start time.Time) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID: id, ICalUID: ical, Title: title,
		StartUTC: start, EndUTC: start.Add(30 * time.Minute), IsBusy: true,
	}
}

// 抽出条件は now < start_utc <= now+lead。終日(is_all_day=1)は対象外(スペック 6 章)。
func TestListUpcomingEvents(t *testing.T) {
	st := remTestStore(t)
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	lead := 10 * time.Minute

	require.NoError(t, st.UpsertEvent(remRef, remEvent("past", "p@x", "過去", now.Add(-time.Minute))))
	require.NoError(t, st.UpsertEvent(remRef, remEvent("at-now", "n@x", "ちょうど今", now)))
	require.NoError(t, st.UpsertEvent(remRef, remEvent("in-window", "w@x", "10分以内", now.Add(5*time.Minute))))
	require.NoError(t, st.UpsertEvent(remRef, remEvent("at-edge", "e@x", "ちょうど境界", now.Add(lead))))
	require.NoError(t, st.UpsertEvent(remRef, remEvent("beyond", "b@x", "遠すぎ", now.Add(lead+time.Second))))
	require.NoError(t, st.UpsertEvent(remRef, model.NormalizedEvent{
		ID: "allday", ICalUID: "a@x", IsAllDay: true,
		AllDayStart: "2026-07-10", AllDayEnd: "2026-07-11", IsBusy: true,
	}))

	got, err := st.ListUpcomingEvents(now, lead)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "in-window", got[0].EventID) // start_utc 昇順
	require.Equal(t, "at-edge", got[1].EventID)
	require.Equal(t, "10分以内", got[0].Title)
	require.Equal(t, "w@x", got[0].ICalUID)
	require.Equal(t, remRef, got[0].Ref)
}

func TestReminderSentLifecycle(t *testing.T) {
	st := remTestStore(t)
	start := time.Date(2026, 7, 10, 9, 5, 0, 0, time.UTC)
	sentAt := time.Date(2026, 7, 10, 8, 55, 0, 0, time.UTC)

	sent, err := st.WasReminderSent(remRef, "ev1", start)
	require.NoError(t, err)
	require.False(t, sent)

	require.NoError(t, st.MarkReminderSent(remRef, "ev1", "u@x", start, sentAt))
	// 冪等(再記録でエラーにならない)
	require.NoError(t, st.MarkReminderSent(remRef, "ev1", "u@x", start, sentAt))

	sent, err = st.WasReminderSent(remRef, "ev1", start)
	require.NoError(t, err)
	require.True(t, sent)

	// iCalUID 照会(dedupe 用)。空 UID は常に false
	dup, err := st.HasReminderForICalUID("u@x", start)
	require.NoError(t, err)
	require.True(t, dup)
	dup, err = st.HasReminderForICalUID("", start)
	require.NoError(t, err)
	require.False(t, dup)

	// 時刻変更で再アーム(start_utc が主キーの一部。スペック 4.3)
	moved := start.Add(time.Hour)
	sent, err = st.WasReminderSent(remRef, "ev1", moved)
	require.NoError(t, err)
	require.False(t, sent)
}

func TestCleanupAndDeleteReminders(t *testing.T) {
	st := remTestStore(t)
	old := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	require.NoError(t, st.MarkReminderSent(remRef, "old", "o@x", old, old))
	require.NoError(t, st.MarkReminderSent(remRef, "recent", "r@x", recent, recent))
	require.NoError(t, st.MarkReminderSent(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, "other", "b@x", recent, recent))

	// start_utc < before の行だけ消える
	require.NoError(t, st.CleanupRemindersSent(time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)))
	sent, err := st.WasReminderSent(remRef, "old", old)
	require.NoError(t, err)
	require.False(t, sent)
	sent, err = st.WasReminderSent(remRef, "recent", recent)
	require.NoError(t, err)
	require.True(t, sent)

	// アカウント削除
	require.NoError(t, st.DeleteRemindersForAccount("b"))
	sent, err = st.WasReminderSent(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, "other", recent)
	require.NoError(t, err)
	require.False(t, sent)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/store/ -race -count=1 -run 'Upcoming|Reminder'`
Expected: FAIL(コンパイルエラー)

- [ ] **Step 3: 実装**

(a) `internal/store/store.go` の const schema 末尾(mappings のインデックスの後)に追加:

```sql
CREATE TABLE IF NOT EXISTS reminders_sent (
  account_id  TEXT NOT NULL,
  calendar_id TEXT NOT NULL,
  event_id    TEXT NOT NULL,
  start_utc   INTEGER NOT NULL,
  ical_uid    TEXT NOT NULL DEFAULT '',
  sent_at     INTEGER NOT NULL,
  PRIMARY KEY (account_id, calendar_id, event_id, start_utc)
);
CREATE INDEX IF NOT EXISTS idx_reminders_sent_icaluid ON reminders_sent (ical_uid, start_utc);
```

(b) 新規 `internal/store/reminders.go`:

```go
package store

import (
	"time"

	"github.com/btajp/calsync/internal/model"
)

// UpcomingEvent は開始前リマインドの対象候補(events キャッシュの時刻指定イベント)。
// キャッシュには busy・未辞退・ウィンドウ内の実予定しか入らない契約のため、
// 抽出側での追加フィルタは不要(スペック 6 章)。
type UpcomingEvent struct {
	Ref      model.CalendarRef
	EventID  string
	ICalUID  string
	Title    string
	StartUTC time.Time
	EndUTC   time.Time
}

// ListUpcomingEvents は「now < start_utc <= now+lead」の時刻指定イベントを
// start_utc 昇順で返す(スペック 6 章の抽出条件。終日は対象外)。
func (s *Store) ListUpcomingEvents(now time.Time, lead time.Duration) ([]UpcomingEvent, error) {
	rows, err := s.db.Query(`
SELECT account_id, calendar_id, event_id, ical_uid, title, start_utc, end_utc
FROM events
WHERE is_all_day = 0 AND start_utc > ? AND start_utc <= ?
ORDER BY start_utc, account_id, calendar_id, event_id`,
		now.UTC().Unix(), now.Add(lead).UTC().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpcomingEvent
	for rows.Next() {
		var (
			u                     UpcomingEvent
			icalUID, title        sql.NullString
			startUnix, endUnix    sql.NullInt64
		)
		if err := rows.Scan(&u.Ref.AccountID, &u.Ref.CalendarID, &u.EventID, &icalUID, &title, &startUnix, &endUnix); err != nil {
			return nil, err
		}
		u.ICalUID, u.Title = icalUID.String, title.String
		if startUnix.Valid {
			u.StartUTC = time.Unix(startUnix.Int64, 0).UTC()
		}
		if endUnix.Valid {
			u.EndUTC = time.Unix(endUnix.Int64, 0).UTC()
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// WasReminderSent は自分自身の送信記録があるかを返す。
func (s *Store) WasReminderSent(ref model.CalendarRef, eventID string, startUTC time.Time) (bool, error) {
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(1) FROM reminders_sent
WHERE account_id = ? AND calendar_id = ? AND event_id = ? AND start_utc = ?`,
		ref.AccountID, ref.CalendarID, eventID, startUTC.UTC().Unix()).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// HasReminderForICalUID は同一 iCalUID+開始時刻の送信記録があるかを返す
// (dedupe_same_meeting 用。スペック 6 章)。icalUID=="" は常に false(dedupe 対象外)。
func (s *Store) HasReminderForICalUID(icalUID string, startUTC time.Time) (bool, error) {
	if icalUID == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(1) FROM reminders_sent WHERE ical_uid = ? AND start_utc = ?`,
		icalUID, startUTC.UTC().Unix()).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkReminderSent は送信記録を upsert する(冪等)。記録条件は
// (a) 送信成功時 (b) dedupe により送信不要と確定した時 の 2 つ(スペック 6 章)。
func (s *Store) MarkReminderSent(ref model.CalendarRef, eventID, icalUID string, startUTC, sentAt time.Time) error {
	_, err := s.db.Exec(`
INSERT INTO reminders_sent (account_id, calendar_id, event_id, start_utc, ical_uid, sent_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (account_id, calendar_id, event_id, start_utc) DO UPDATE SET
  ical_uid = excluded.ical_uid,
  sent_at  = excluded.sent_at`,
		ref.AccountID, ref.CalendarID, eventID, startUTC.UTC().Unix(), icalUID, sentAt.UTC().Unix())
	return err
}

// CleanupRemindersSent は start_utc < before の行を削除する(日次リコンサイル相乗り。
// スペック 4.3: before = now-48h)。
func (s *Store) CleanupRemindersSent(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM reminders_sent WHERE start_utc < ?`, before.UTC().Unix())
	return err
}

// DeleteRemindersForAccount はアカウントの送信記録を削除する(accounts remove 用)。
func (s *Store) DeleteRemindersForAccount(accountID string) error {
	_, err := s.db.Exec(`DELETE FROM reminders_sent WHERE account_id = ?`, accountID)
	return err
}
```

※ `sql.NullString` を使うため import に `"database/sql"` を追加すること。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/store/ -race -count=1`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/store/
git commit -m "feat: add reminders_sent table and upcoming-event queries"
```

---

### Task 6: notify sentinel と engine の Notifier 型を定義

**Files:**
- Create: `internal/notify/notify.go`
- Create: `internal/engine/notify.go`(この Task では型定義のみ)
- Modify: `internal/engine/engine.go`(Engine 構造体に Notifier フィールド)
- Test: 型定義のみのためコンパイル確認(`go build ./...`)

**Interfaces:**
- Produces(Task 7〜10 が使う):
  - `notify.ErrNonRetryable`(sentinel。`errors.Is` で分類)
  - `engine.DigestEntry{ Title string; StartUTC, EndUTC time.Time; IsAllDay bool; AllDayStart string; AccountIDs []string }`
  - `engine.Notifier interface { SendDigest(ctx, day, entries, failedAccounts) error; SendReminder(ctx, e, lead) error }`
  - `engine.Engine.Notifier Notifier` フィールド(nil で通知無効)

- [ ] **Step 1: 実装**(型定義のみのため TDD 対象外。コンパイルが検証)

(a) 新規 `internal/notify/notify.go`:

```go
// Package notify は通知送信のエラー分類(sentinel)を提供する。
// 送信実装の方言(Slack API の ok:false + error 文字列等)は各サブパッケージに
// 閉じ込め、エンジンは errors.Is(err, ErrNonRetryable) だけで再試行可否を判断する
// (provider の autherr と同じ「方言を漏らさない」方針。スペック 8 章)。
package notify

import "errors"

// ErrNonRetryable は再試行しても回復しない送信エラー(設定起因等)。
// これにマッチしないエラーはリトライ可能(ネットワーク・5xx・429)として扱う。
var ErrNonRetryable = errors.New("non-retryable notification error")
```

(b) 新規 `internal/engine/notify.go`:

```go
package engine

import (
	"context"
	"time"
)

// DigestEntry は通知 1 件分の構造化データ。文言の組み立て(フォーマット・
// エスケープ)は Notifier 実装(internal/notify/slack)の責務で、エンジンは
// データだけを渡す(スペック 7 章)。
type DigestEntry struct {
	Title       string
	StartUTC    time.Time
	EndUTC      time.Time
	IsAllDay    bool
	AllDayStart string
	AccountIDs  []string // dedupe 統合後の由来アカウント(YAML の id)
}

// Notifier は通知送信のインターフェース。Engine.Notifier が nil なら通知機能は
// 完全に無効(スペック 9 章)。エラーは notify.ErrNonRetryable への包み込みで
// リトライ可否を表現すること。
type Notifier interface {
	SendDigest(ctx context.Context, day time.Time, entries []DigestEntry, failedAccounts []string) error
	// lead は送信時点の実残り時間(スペック 7 章)
	SendReminder(ctx context.Context, e DigestEntry, lead time.Duration) error
}
```

(c) `internal/engine/engine.go` の Engine 構造体に追加(`Now` の下):

```go
	Notifier  Notifier         // nil なら通知無効(cmd_run のみが注入する)
```

- [ ] **Step 2: コンパイルと全テストの確認**

Run: `go build ./... && go test ./... -race -count=1`
Expected: PASS(既存テストは Notifier nil のままなので影響ゼロ)

- [ ] **Step 3: コミット**

```bash
git add internal/notify/notify.go internal/engine/notify.go internal/engine/engine.go
git commit -m "feat: define Notifier interface and non-retryable error sentinel"
```

---

### Task 7: internal/notify/slack パッケージ(フォーマット+送信)

**Files:**
- Create: `internal/notify/slack/format.go`
- Create: `internal/notify/slack/slack.go`
- Test: `internal/notify/slack/format_test.go`、`internal/notify/slack/slack_test.go`

**Interfaces:**
- Consumes: `engine.DigestEntry` / `engine.Notifier`(Task 6)、`notify.ErrNonRetryable`
- Produces: `slack.New(token, channel string) *Client`(`engine.Notifier` を満たす。公開フィールド `BaseURL`(テスト用)と `TZ *time.Location`(nil なら time.Local))

- [ ] **Step 1: 失敗するテストを書く(フォーマット)**

新規 `internal/notify/slack/format_test.go`:

```go
package slack

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/engine"
)

var jst = time.FixedZone("JST", 9*3600)

// 2026-07-05 は日曜(JST)。
var digestDay = time.Date(2026, 7, 5, 0, 0, 0, 0, jst)

func entry(title string, startJST, endJST time.Time, accts ...string) engine.DigestEntry {
	return engine.DigestEntry{Title: title, StartUTC: startJST.UTC(), EndUTC: endJST.UTC(), AccountIDs: accts}
}

func TestFormatDigest(t *testing.T) {
	entries := []engine.DigestEntry{
		{Title: "社内イベント", IsAllDay: true, AllDayStart: "2026-07-05", AccountIDs: []string{"work-google"}},
		entry("朝会", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 9, 30, 0, 0, jst), "work-google"),
		entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "work-ms", "personal"),
	}
	got := formatDigest(digestDay, entries, nil, jst)
	want := "📅 7/5(日) の予定\n" +
		"・(終日) 社内イベント [work-google]\n" +
		"・09:00–09:30 朝会 [work-google]\n" +
		"・10:00–11:00 設計レビュー [work-ms, personal]"
	require.Equal(t, want, got)
}

func TestFormatDigestEmptyAndFailed(t *testing.T) {
	got := formatDigest(digestDay, nil, []string{"acct-x"}, jst)
	require.Contains(t, got, "今日の予定はありません")
	require.Contains(t, got, "⚠ acct-x: 取得失敗")
}

// 件名は外部入力(会議招待)由来。<!channel> をエスケープしないと全員メンションが
// 発火する(メンションインジェクション。スペック 8 章)。
func TestFormatEscapesMentionInjection(t *testing.T) {
	e := entry("<!channel> attack & <evil>", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a")
	got := formatDigest(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.NotContains(t, got, "<!channel>")
	require.Contains(t, got, "&lt;!channel&gt; attack &amp; &lt;evil&gt;")
}

// 当日ウィンドウ外にはみ出す側に日付を付ける(スペック 7 章)。
func TestFormatCrossMidnight(t *testing.T) {
	e := entry("夜勤", time.Date(2026, 7, 4, 23, 0, 0, 0, jst), time.Date(2026, 7, 6, 1, 0, 0, 0, jst), "a")
	got := formatDigest(digestDay, []engine.DigestEntry{e}, nil, jst)
	require.Contains(t, got, "7/4 23:00–7/6 01:00")
}

func TestFormatDigestCapsAt100(t *testing.T) {
	var entries []engine.DigestEntry
	for i := 0; i < 105; i++ {
		entries = append(entries, entry("e", time.Date(2026, 7, 5, 9, 0, 0, 0, jst), time.Date(2026, 7, 5, 10, 0, 0, 0, jst), "a"))
	}
	got := formatDigest(digestDay, entries, nil, jst)
	require.Contains(t, got, "…他 5 件")
}

func TestFormatReminder(t *testing.T) {
	e := entry("設計レビュー", time.Date(2026, 7, 5, 10, 0, 0, 0, jst), time.Date(2026, 7, 5, 11, 0, 0, 0, jst), "work-ms")
	got := formatReminder(e, 8*time.Minute, jst)
	require.Equal(t, "⏰ 8分後: 10:00–11:00 設計レビュー [work-ms]", got)

	// 1 分未満は「まもなく」
	got = formatReminder(e, 20*time.Second, jst)
	require.Contains(t, got, "まもなく")

	// 無題は「(件名なし)」(スペック 7 章)
	e.Title = ""
	got = formatReminder(e, 8*time.Minute, jst)
	require.Contains(t, got, "(件名なし)")
}
```

- [ ] **Step 2: 失敗するテストを書く(送信・エラー分類)**

新規 `internal/notify/slack/slack_test.go`:

```go
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/notify"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New("xoxb-test", "C123")
	c.BaseURL = srv.URL
	c.TZ = time.UTC
	return c
}

func sampleEntry() engine.DigestEntry {
	return engine.DigestEntry{
		Title:      "e",
		StartUTC:   time.Date(2026, 7, 5, 1, 0, 0, 0, time.UTC),
		EndUTC:     time.Date(2026, 7, 5, 2, 0, 0, 0, time.UTC),
		AccountIDs: []string{"a"},
	}
}

func TestSendReminderPostsMessage(t *testing.T) {
	var got struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat.postMessage", r.URL.Path)
		require.Equal(t, "Bearer xoxb-test", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"ok":true}`))
	})
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), 10*time.Minute))
	require.Equal(t, "C123", got.Channel)
	require.Contains(t, got.Text, "10分後")
}

// ok:false は未知のエラー文字列も含め既定でリトライ不能(スペック 8 章)。
func TestAPIErrorsAreNonRetryable(t *testing.T) {
	for _, apiErr := range []string{"invalid_auth", "channel_not_found", "some_future_error"} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"ok":false,"error":"` + apiErr + `"}`))
		})
		err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
		require.Error(t, err)
		require.True(t, errors.Is(err, notify.ErrNonRetryable), "error %q should be non-retryable", apiErr)
	}
}

// 429 / 5xx / ネットワークエラーはリトライ可能(sentinel を含まない)。
func TestTransportErrorsAreRetryable(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
		require.Error(t, err)
		require.False(t, errors.Is(err, notify.ErrNonRetryable), "status %d should be retryable", status)
	}

	c := New("xoxb-test", "C123")
	c.BaseURL = "http://127.0.0.1:1" // 到達不能
	err := c.SendReminder(context.Background(), sampleEntry(), time.Minute)
	require.Error(t, err)
	require.False(t, errors.Is(err, notify.ErrNonRetryable))
}

// U… は conversations.open で DM に解決し、プロセス存続中はキャッシュする(スペック 8 章)。
func TestDMResolutionIsCached(t *testing.T) {
	var opens atomic.Int64
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.open":
			opens.Add(1)
			w.Write([]byte(`{"ok":true,"channel":{"id":"D999"}}`))
		case "/chat.postMessage":
			var body struct {
				Channel string `json:"channel"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "D999", body.Channel)
			w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	c.Channel = "U777"
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), time.Minute))
	require.NoError(t, c.SendReminder(context.Background(), sampleEntry(), time.Minute))
	require.Equal(t, int64(1), opens.Load())
}
```

- [ ] **Step 3: テストが失敗することを確認**

Run: `go test ./internal/notify/... -race -count=1`
Expected: FAIL(コンパイルエラー)

- [ ] **Step 4: 実装**

(a) 新規 `internal/notify/slack/format.go`:

```go
package slack

import (
	"fmt"
	"strings"
	"time"

	"github.com/btajp/calsync/internal/engine"
)

// maxDigestEntries は 1 メッセージに載せるエントリ上限(Slack メッセージ長対策。スペック 5 章)。
const maxDigestEntries = 100

var jaWeekdays = [...]string{"日", "月", "火", "水", "木", "金", "土"}

// escapeText は Slack text の必須エスケープ(& < > のみ。スペック 8 章)。
// 件名は外部入力(会議招待)由来で、素通しすると <!channel> 等の特殊メンション
// 構文が発火する(メンションインジェクション)。
func escapeText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// titleOf は表示用件名を返す。空(無題または title 未反映の移行期間)は
// 「(件名なし)」で両ケースを区別しない(スペック 7 章)。
func titleOf(e engine.DigestEntry) string {
	if e.Title == "" {
		return "(件名なし)"
	}
	return escapeText(e.Title)
}

func accountsLabel(ids []string) string {
	return escapeText("[" + strings.Join(ids, ", ") + "]")
}

// timeRange は開始–終了を表示する。当日ウィンドウ外にはみ出す側には日付を付ける
// (例: "7/4 23:00–7/6 01:00"。スペック 7 章)。
func timeRange(e engine.DigestEntry, day time.Time, loc *time.Location) string {
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	s, en := e.StartUTC.In(loc), e.EndUTC.In(loc)
	start := s.Format("15:04")
	if s.Before(dayStart) {
		start = fmt.Sprintf("%d/%d %s", int(s.Month()), s.Day(), start)
	}
	end := en.Format("15:04")
	if en.After(dayEnd) {
		end = fmt.Sprintf("%d/%d %s", int(en.Month()), en.Day(), end)
	}
	return start + "–" + end
}

func formatDigest(day time.Time, entries []engine.DigestEntry, failedAccounts []string, loc *time.Location) string {
	var b strings.Builder
	d := day.In(loc)
	fmt.Fprintf(&b, "📅 %d/%d(%s) の予定\n", int(d.Month()), d.Day(), jaWeekdays[d.Weekday()])
	if len(entries) == 0 {
		// 0 件の日も送る(デーモンの生存確認を兼ねる。スペック 5 章)
		b.WriteString("今日の予定はありません\n")
	}
	shown := entries
	if len(shown) > maxDigestEntries {
		shown = shown[:maxDigestEntries]
	}
	for _, e := range shown {
		if e.IsAllDay {
			fmt.Fprintf(&b, "・(終日) %s %s\n", titleOf(e), accountsLabel(e.AccountIDs))
			continue
		}
		fmt.Fprintf(&b, "・%s %s %s\n", timeRange(e, d, loc), titleOf(e), accountsLabel(e.AccountIDs))
	}
	if n := len(entries) - len(shown); n > 0 {
		fmt.Fprintf(&b, "…他 %d 件\n", n)
	}
	for _, id := range failedAccounts {
		// 取得に失敗したアカウントは黙って欠落させない(スペック 5 章)
		fmt.Fprintf(&b, "⚠ %s: 取得失敗\n", escapeText(id))
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatReminder の「N 分後」は送信時点の実残り時間(分丸め)。1 分未満は「まもなく」
// (遅延通知・再起動復帰時に設定値と実態がずれるため設定値は表示しない。スペック 7 章)。
func formatReminder(e engine.DigestEntry, lead time.Duration, loc *time.Location) string {
	mins := int(lead.Round(time.Minute) / time.Minute)
	prefix := "まもなく"
	if mins >= 1 {
		prefix = fmt.Sprintf("%d分後", mins)
	}
	day := e.StartUTC.In(loc)
	return fmt.Sprintf("⏰ %s: %s %s %s", prefix, timeRange(e, day, loc), titleOf(e), accountsLabel(e.AccountIDs))
}
```

(b) 新規 `internal/notify/slack/slack.go`:

```go
// Package slack は Slack Web API(chat.postMessage / conversations.open)への
// 通知送信を実装する。依存追加なし(net/http 直叩き。スペック 8 章)。
// Slack の方言(ok:false + error 文字列)はこのパッケージから漏らさない:
// リトライ可否は notify.ErrNonRetryable への包み込みで表現する。
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/notify"
)

// コンパイル時に engine.Notifier を満たすことを保証する。
var _ engine.Notifier = (*Client)(nil)

// Client は engine.Notifier の Slack 実装。
type Client struct {
	Token   string
	Channel string         // C…/G…: そのまま / U…: conversations.open で DM に解決
	BaseURL string         // テスト用。空なら https://slack.com/api
	TZ      *time.Location // 表示 TZ。nil なら time.Local(コンテナ TZ)

	httpc *http.Client

	mu       sync.Mutex
	resolved string // U… を解決した DM チャンネル ID のキャッシュ(プロセス存続中)
}

func New(token, channel string) *Client {
	// 1 リクエスト 10 秒タイムアウト: Run ループ内の同期呼び出しのため上限を保証する(スペック 8 章)
	return &Client{Token: token, Channel: channel, httpc: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) SendDigest(ctx context.Context, day time.Time, entries []engine.DigestEntry, failedAccounts []string) error {
	return c.post(ctx, formatDigest(day, entries, failedAccounts, c.loc()))
}

func (c *Client) SendReminder(ctx context.Context, e engine.DigestEntry, lead time.Duration) error {
	return c.post(ctx, formatReminder(e, lead, c.loc()))
}

func (c *Client) loc() *time.Location {
	if c.TZ != nil {
		return c.TZ
	}
	return time.Local
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://slack.com/api"
}

type apiResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
}

// call は Slack Web API を 1 回呼ぶ。分類規則(スペック 8 章):
//   - ネットワークエラー・5xx・429 → リトライ可能(sentinel を含まない)
//   - それ以外の非 2xx・ok:false(未知のエラー文字列含む)→ notify.ErrNonRetryable
func (c *Client) call(ctx context.Context, method string, payload any) (*apiResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("slack %s: encode: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("slack %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack %s: %w", method, err) // ネットワーク系 → リトライ可能
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("slack %s: status %d", method, resp.StatusCode) // リトライ可能
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack %s: status %d: %w", method, resp.StatusCode, notify.ErrNonRetryable)
	}
	var ar apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, fmt.Errorf("slack %s: decode: %w", method, err)
	}
	if !ar.OK {
		return nil, fmt.Errorf("slack %s: %s: %w", method, ar.Error, notify.ErrNonRetryable)
	}
	return &ar, nil
}

// channelID は投稿先チャンネル ID を返す。U… は初回のみ conversations.open で
// DM チャンネルに解決し、プロセス存続中はキャッシュする(スペック 8 章)。
func (c *Client) channelID(ctx context.Context) (string, error) {
	if !strings.HasPrefix(c.Channel, "U") {
		return c.Channel, nil
	}
	c.mu.Lock()
	cached := c.resolved
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	ar, err := c.call(ctx, "conversations.open", map[string]string{"users": c.Channel})
	if err != nil {
		return "", err
	}
	if ar.Channel.ID == "" {
		return "", fmt.Errorf("slack conversations.open: empty channel id: %w", notify.ErrNonRetryable)
	}
	c.mu.Lock()
	c.resolved = ar.Channel.ID
	c.mu.Unlock()
	return ar.Channel.ID, nil
}

func (c *Client) post(ctx context.Context, text string) error {
	ch, err := c.channelID(ctx)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, "chat.postMessage", map[string]string{"channel": ch, "text": text})
	return err
}
```

- [ ] **Step 5: テストが通ることを確認**

Run: `go test ./internal/notify/... -race -count=1`
Expected: PASS

- [ ] **Step 6: コミット**

```bash
git add internal/notify/
git commit -m "feat: add Slack notifier (chat.postMessage, DM resolution, escaping, error classes)"
```

---

### Task 8: engine のダイジェスト収集と runDigest

**Files:**
- Modify: `internal/engine/notify.go`(collectDigest / digestIncludes / appendDigestEntry / sortDigestEntries / runDigest / digestAt を追加)
- Test: `internal/engine/notify_test.go`(新規)

**Interfaces:**
- Consumes: Task 1〜4・6 の全成果物
- Produces(Task 9 が使う):
  - `func (e *Engine) runDigest(ctx context.Context, scheduled time.Time) time.Time`
  - `func (e *Engine) digestAt() string`(無効なら "")

- [ ] **Step 1: 失敗するテストを書く**

新規 `internal/engine/notify_test.go`:

```go
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/notify"
	"github.com/btajp/calsync/internal/store"
)

// fakeNotifier は Notifier のテストダブル。failWith は次の Send を 1 回失敗させる。
type fakeNotifier struct {
	mu        sync.Mutex
	digests   []fakeDigest
	reminders []fakeReminder
	failWith  error
}

type fakeDigest struct {
	day     time.Time
	entries []DigestEntry
	failed  []string
}

type fakeReminder struct {
	entry DigestEntry
	lead  time.Duration
}

func (f *fakeNotifier) SendDigest(ctx context.Context, day time.Time, entries []DigestEntry, failedAccounts []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		err := f.failWith
		f.failWith = nil
		return err
	}
	f.digests = append(f.digests, fakeDigest{day: day, entries: entries, failed: failedAccounts})
	return nil
}

func (f *fakeNotifier) SendReminder(ctx context.Context, e DigestEntry, lead time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		err := f.failWith
		f.failWith = nil
		return err
	}
	f.reminders = append(f.reminders, fakeReminder{entry: e, lead: lead})
	return nil
}

var jstLoc = time.FixedZone("JST", 9*3600)

// digestEngine は JST の 2026-07-05 07:30 を now とするエンジンを組み立てる。
func digestEngine(t *testing.T) (*Engine, *fake.Fake, *fakeNotifier) {
	t.Helper()
	e, f := newTestEngine(t)
	fn := &fakeNotifier{}
	e.Notifier = fn
	e.Cfg.Notifications.Slack = &config.SlackConfig{
		Channel: "C1", MorningDigest: "07:30", RemindBefore: 10 * time.Minute,
	}
	e.Now = func() time.Time { return time.Date(2026, 7, 5, 7, 30, 0, 0, jstLoc) }
	return e, f, fn
}

func timedEvent(id, ical, title string, startJST time.Time, busy bool) model.NormalizedEvent {
	return model.NormalizedEvent{
		ID: id, ICalUID: ical, Title: title,
		StartUTC: startJST.UTC(), EndUTC: startJST.Add(time.Hour).UTC(), IsBusy: busy,
	}
}

func TestCollectDigestFiltersAndSorts(t *testing.T) {
	e, f, _ := digestEngine(t)
	ctx := context.Background()
	day := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)

	// アカウント a のライブ取得結果(fake は cursor=="" で full を返す)
	f.SetFullState(refA, []model.NormalizedEvent{
		timedEvent("late", "late@x", "午後の予定", time.Date(2026, 7, 5, 14, 0, 0, 0, jstLoc), true),
		timedEvent("early", "early@x", "朝会", time.Date(2026, 7, 5, 9, 0, 0, 0, jstLoc), true),
		timedEvent("free", "free@x", "free も含む", time.Date(2026, 7, 5, 11, 0, 0, 0, jstLoc), false),
		func() model.NormalizedEvent {
			ev := timedEvent("declined", "d@x", "辞退済み", time.Date(2026, 7, 5, 12, 0, 0, 0, jstLoc), true)
			ev.IsDeclined = true
			return ev
		}(),
		func() model.NormalizedEvent {
			ev := timedEvent("tagged", "t@x", "タグ二次判定", time.Date(2026, 7, 5, 13, 0, 0, 0, jstLoc), true)
			ev.OriginTag = "x:orig1"
			return ev
		}(),
		timedEvent("tomorrow", "tm@x", "翌日", time.Date(2026, 7, 6, 9, 0, 0, 0, jstLoc), true),
		// 前日(7/4)の終日予定: Window.Contains の UTC 近似だと JST で混入する(除外されること)
		{ID: "ad-prev", ICalUID: "ap@x", Title: "前日の終日", IsAllDay: true, AllDayStart: "2026-07-04", AllDayEnd: "2026-07-05", IsBusy: true},
		{ID: "ad-today", ICalUID: "at@x", Title: "当日の終日", IsAllDay: true, AllDayStart: "2026-07-05", AllDayEnd: "2026-07-06", IsBusy: true},
		timedEvent("blocker", "b@x", "受領ブロッカー", time.Date(2026, 7, 5, 10, 0, 0, 0, jstLoc), true),
	})
	// mappings 一次判定: アカウント a 上の "blocker" は受領ブロッカー
	require.NoError(t, e.Store.PutMapping(store.Mapping{
		OriginAccount: "b", OriginCalendar: "primary", OriginEventID: "src1",
		TargetAccount: "a", TargetCalendar: "primary", BlockerEventID: "blocker",
		IdempotencyKey: "k1", TimeHash: "h1", Status: store.StatusActive,
	}))

	entries, failed := e.collectDigest(ctx, day)
	require.Empty(t, failed)

	var titles []string
	for _, en := range entries {
		titles = append(titles, en.Title)
	}
	// 終日が先頭、以降開始時刻順。free は含む。前日終日・辞退・タグ付き・ブロッカー・翌日は除外
	require.Equal(t, []string{"当日の終日", "朝会", "free も含む", "午後の予定"}, titles)
}

func TestCollectDigestDedupesAcrossAccounts(t *testing.T) {
	e, f, _ := digestEngine(t)
	day := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	start := time.Date(2026, 7, 5, 10, 0, 0, 0, jstLoc)

	f.SetFullState(refA, []model.NormalizedEvent{timedEvent("ev-a", "same@x", "設計レビュー", start, true)})
	f.SetFullState(model.CalendarRef{AccountID: "b", CalendarID: "primary"},
		[]model.NormalizedEvent{timedEvent("ev-b", "same@x", "", start, true)}) // b 側は無題
	f.SetFullState(model.CalendarRef{AccountID: "c", CalendarID: "primary"}, nil)

	entries, failed := e.collectDigest(context.Background(), day)
	require.Empty(t, failed)
	require.Len(t, entries, 1)
	require.Equal(t, "設計レビュー", entries[0].Title) // 設定順で最初の非空 Title
	require.Equal(t, []string{"a", "b"}, entries[0].AccountIDs)
}

func TestCollectDigestReportsFailedAccounts(t *testing.T) {
	e, f, _ := digestEngine(t)
	day := time.Date(2026, 7, 5, 0, 0, 0, 0, jstLoc)
	f.SetFullState(refA, []model.NormalizedEvent{timedEvent("ok", "ok@x", "生きてる", time.Date(2026, 7, 5, 9, 0, 0, 0, jstLoc), true)})
	f.FailNext(model.CalendarRef{AccountID: "b", CalendarID: "primary"}, errors.New("boom"))

	entries, failed := e.collectDigest(context.Background(), day)
	require.Equal(t, []string{"b"}, failed)
	require.Len(t, entries, 1)
}

func TestRunDigest(t *testing.T) {
	e, f, fn := digestEngine(t)
	f.SetFullState(refA, nil)
	scheduled := time.Date(2026, 7, 5, 7, 30, 0, 0, jstLoc)

	// 成功 → 翌日 07:30 を返す
	next := e.runDigest(context.Background(), scheduled)
	require.Len(t, fn.digests, 1)
	require.Equal(t, time.Date(2026, 7, 6, 7, 30, 0, 0, jstLoc).Unix(), next.Unix())

	// リトライ可能エラー → scheduled 据え置き
	fn.failWith = errors.New("network down")
	next = e.runDigest(context.Background(), scheduled)
	require.Equal(t, scheduled.Unix(), next.Unix())
	require.Len(t, fn.digests, 1)

	// リトライ不能エラー → 翌日へ進める
	fn.failWith = fmt.Errorf("invalid_auth: %w", notify.ErrNonRetryable)
	next = e.runDigest(context.Background(), scheduled)
	require.Equal(t, time.Date(2026, 7, 6, 7, 30, 0, 0, jstLoc).Unix(), next.Unix())
	require.Len(t, fn.digests, 1)

	// 対象日が過去日(跨日遅延)→ 送らず放棄して翌日へ(スペック 9 章)
	stale := time.Date(2026, 7, 4, 7, 30, 0, 0, jstLoc)
	next = e.runDigest(context.Background(), stale)
	require.Len(t, fn.digests, 1)
	require.Equal(t, time.Date(2026, 7, 6, 7, 30, 0, 0, jstLoc).Unix(), next.Unix())
}
```

※ import の `fake` / `config` は `newTestEngine` が返す型に合わせる(`engine_test.go` と同じ import 群)。fake の内部状態には触れず、必ず公開 API(`SetFullState` / `FailNext`)だけを使うこと。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/engine/ -race -count=1 -run 'Digest'`
Expected: FAIL(コンパイルエラー)

- [ ] **Step 3: 実装**

`internal/engine/notify.go` に追加(import に `log` / `sort` / `errors` / `github.com/btajp/calsync/internal/model` / `github.com/btajp/calsync/internal/notify` を追加):

```go
// digestAt はダイジェストが有効なとき発火時刻("HH:MM")を、無効なら "" を返す。
func (e *Engine) digestAt() string {
	if e.Notifier == nil || e.Cfg.Notifications.Slack == nil {
		return ""
	}
	return e.Cfg.Notifications.Slack.MorningDigest
}

// runDigest は 1 回分のダイジェスト送信を実行し、次回の発火時刻を返す。
// 対象日は now ではなく scheduled(予定していた発火時刻)の日付から導出する
// (発火が midnight を跨いで遅延しても対象日がずれず、同日 2 通を防ぐ。スペック 5 章)。
// エラーポリシー(スペック 9 章): リトライ可能 → scheduled 据え置きで次 tick 再試行。
// リトライ不能 → 翌日へ。対象日が過去日になったら放棄して翌日へ。
func (e *Engine) runDigest(ctx context.Context, scheduled time.Time) time.Time {
	hhmm := e.digestAt()
	now := e.now()
	day := time.Date(scheduled.Year(), scheduled.Month(), scheduled.Day(), 0, 0, 0, 0, scheduled.Location())
	nowLocal := now.In(scheduled.Location())
	nowDay := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, scheduled.Location())
	if nowDay.After(day) {
		log.Printf("digest: abandoning stale digest for %s", day.Format("2006-01-02"))
		return nextDailyAt(now, hhmm)
	}
	entries, failed := e.collectDigest(ctx, day)
	if err := e.Notifier.SendDigest(ctx, day, entries, failed); err != nil {
		if errors.Is(err, notify.ErrNonRetryable) {
			log.Printf("digest: %v (non-retryable; skipping until tomorrow)", err)
			return nextDailyAt(now, hhmm)
		}
		log.Printf("digest: %v (retrying next tick)", err)
		return scheduled
	}
	return nextDailyAt(now, hhmm)
}

// collectDigest は対象日 day(そのローカル TZ の 00:00)の実予定をライブ取得で
// 収集する(スペック 5 章)。newCursor は捨てる(カーソル規律に抵触しない)。
// 戻り値 failed は取得に失敗したアカウント ID(設定順・重複なし)。
func (e *Engine) collectDigest(ctx context.Context, day time.Time) ([]DigestEntry, []string) {
	dayStart := day
	dayEnd := day.AddDate(0, 0, 1)
	dayStr := day.Format("2006-01-02")
	w := model.Window{Start: dayStart, End: dayEnd}

	var (
		entries []DigestEntry
		failed  []string
	)
	byKey := make(map[string]int)
	for _, acct := range e.Cfg.Accounts {
		acctFailed := false
		p, err := e.providerFor(acct.ID)
		if err != nil {
			log.Printf("digest %s: %v", acct.ID, err)
			failed = append(failed, acct.ID)
			continue
		}
		for _, calID := range acct.Calendars {
			if ctx.Err() != nil {
				return entries, failed
			}
			ref := model.CalendarRef{AccountID: acct.ID, CalendarID: calID}
			evs, _, err := p.Changes(ctx, ref, "", w)
			if err != nil {
				log.Printf("digest %s: %v", ref, err)
				acctFailed = true
				break
			}
			for _, ev := range evs {
				include, err := e.digestIncludes(ref, ev, dayStart, dayEnd, dayStr)
				if err != nil {
					log.Printf("digest %s: %v", ref, err)
					acctFailed = true
					break
				}
				if include {
					e.appendDigestEntry(&entries, byKey, acct.ID, ev)
				}
			}
			if acctFailed {
				break
			}
		}
		if acctFailed {
			failed = append(failed, acct.ID)
		}
	}
	sortDigestEntries(entries)
	return entries, failed
}

// digestIncludes は 1 イベントがダイジェスト対象かを判定する(スペック 5 章)。
// 除外: 削除・辞退・ブロッカー(mappings 一次 + タグ二次。Graph delta はタグを
// 返せないため二次判定はライブ取得経路では Google のみ有効)。free は含める。
// 当日判定: 時刻指定は UTC 重なり、終日は現地日付の文字列比較。
// Window.Contains の終日 UTC 近似は 1 日幅では前日/翌日を誤包含するため使わない。
func (e *Engine) digestIncludes(ref model.CalendarRef, ev model.NormalizedEvent, dayStart, dayEnd time.Time, dayStr string) (bool, error) {
	if ev.Deleted || ev.IsDeclined || ev.OriginTag != "" {
		return false, nil
	}
	isBlocker, err := e.Store.IsBlocker(ref.AccountID, ev.ID)
	if err != nil {
		return false, err
	}
	if isBlocker {
		return false, nil
	}
	if ev.IsAllDay {
		if ev.AllDayStart == "" {
			return false, nil
		}
		if ev.AllDayEnd == "" {
			return ev.AllDayStart == dayStr, nil
		}
		return ev.AllDayStart <= dayStr && dayStr < ev.AllDayEnd, nil
	}
	return ev.EndUTC.After(dayStart) && ev.StartUTC.Before(dayEnd), nil
}

// appendDigestEntry は dedupe_same_meeting=true のとき同一 iCalUID+開始時刻の
// エントリを 1 行に統合する(由来アカウント併記。件名は設定順で最初の非空を採用
// する決定的規則。スペック 5 章)。
func (e *Engine) appendDigestEntry(entries *[]DigestEntry, byKey map[string]int, accountID string, ev model.NormalizedEvent) {
	entry := DigestEntry{
		Title:       ev.Title,
		StartUTC:    ev.StartUTC,
		EndUTC:      ev.EndUTC,
		IsAllDay:    ev.IsAllDay,
		AllDayStart: ev.AllDayStart,
		AccountIDs:  []string{accountID},
	}
	if !e.Cfg.DedupeSameMeeting || ev.ICalUID == "" {
		*entries = append(*entries, entry)
		return
	}
	key := ev.ICalUID + "|" + ev.AllDayStart + "|" + ev.StartUTC.UTC().Format(time.RFC3339)
	if i, ok := byKey[key]; ok {
		ex := &(*entries)[i]
		if !slices.Contains(ex.AccountIDs, accountID) {
			ex.AccountIDs = append(ex.AccountIDs, accountID)
		}
		if ex.Title == "" && ev.Title != "" {
			ex.Title = ev.Title
		}
		return
	}
	byKey[key] = len(*entries)
	*entries = append(*entries, entry)
}

// sortDigestEntries は終日を先頭に、次いで開始時刻昇順・件名で並べる(決定的)。
func sortDigestEntries(entries []DigestEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.IsAllDay != b.IsAllDay {
			return a.IsAllDay
		}
		if a.IsAllDay {
			if a.AllDayStart != b.AllDayStart {
				return a.AllDayStart < b.AllDayStart
			}
			return a.Title < b.Title
		}
		if !a.StartUTC.Equal(b.StartUTC) {
			return a.StartUTC.Before(b.StartUTC)
		}
		return a.Title < b.Title
	})
}
```

※ `slices` は標準ライブラリ(Go 1.25)。import に追加。`nextDailyAt` は Task 9 でリネームするが、この Task の時点ではまだ `nextReconcileAt` なので、**この Task では `nextReconcileAt` を呼び、Task 9 のリネーム時に一括置換する**(コンパイルを常に通すため)。

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/engine/ -race -count=1`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/engine/
git commit -m "feat: collect and send the morning digest via live fetch"
```

---

### Task 9: checkReminders と Run ループ統合

**Files:**
- Modify: `internal/engine/scheduler.go`(`nextReconcileAt` → `nextDailyAt` リネーム+Run ループ変更)
- Modify: `internal/engine/notify.go`(checkReminders 追加、`nextReconcileAt` 参照の更新)
- Modify: `internal/engine/scheduler_test.go`(`TestNextReconcileAt` のリネーム追従)
- Test: `internal/engine/notify_test.go`(checkReminders のテスト追加)

**Interfaces:**
- Consumes: Task 5 の store API、Task 8 の runDigest
- Produces: `func (e *Engine) checkReminders(ctx context.Context)`、Run ループの digest/reminder 統合

- [ ] **Step 1: 失敗するテストを書く**

`internal/engine/notify_test.go` に追記:

```go
// reminderEngine は now を可変にしたリマインド用エンジン。
func reminderEngine(t *testing.T) (*Engine, *fakeNotifier, *time.Time) {
	t.Helper()
	e, _ := newTestEngine(t)
	fn := &fakeNotifier{}
	e.Notifier = fn
	e.Cfg.Notifications.Slack = &config.SlackConfig{Channel: "C1", RemindBefore: 10 * time.Minute}
	now := time.Date(2026, 7, 5, 9, 50, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	return e, fn, &now
}

func TestCheckRemindersSendsOncePersistently(t *testing.T) {
	e, fn, _ := reminderEngine(t)
	ctx := context.Background()
	start := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC) // ちょうど 10 分後(境界: 送信対象)
	require.NoError(t, e.Store.UpsertEvent(refA, model.NormalizedEvent{
		ID: "ev1", ICalUID: "u@x", Title: "設計レビュー",
		StartUTC: start, EndUTC: start.Add(time.Hour), IsBusy: true,
	}))

	e.checkReminders(ctx)
	require.Len(t, fn.reminders, 1)
	require.Equal(t, "設計レビュー", fn.reminders[0].entry.Title)
	require.Equal(t, 10*time.Minute, fn.reminders[0].lead)

	// 同一 tick 再実行・再起動(新 Engine 値・同一 Store)でも再送しない
	e.checkReminders(ctx)
	require.Len(t, fn.reminders, 1)
	e2 := &Engine{Store: e.Store, Providers: e.Providers, Cfg: e.Cfg, Now: e.Now, Notifier: fn}
	e2.checkReminders(ctx)
	require.Len(t, fn.reminders, 1)
}

func TestCheckRemindersWindowBoundaries(t *testing.T) {
	e, fn, _ := reminderEngine(t)
	ctx := context.Background()
	// start == now は対象外(start_utc > now)
	require.NoError(t, e.Store.UpsertEvent(refA, model.NormalizedEvent{
		ID: "now", ICalUID: "n@x", StartUTC: time.Date(2026, 7, 5, 9, 50, 0, 0, time.UTC),
		EndUTC: time.Date(2026, 7, 5, 10, 50, 0, 0, time.UTC), IsBusy: true,
	}))
	// 10 分 1 秒後は対象外
	require.NoError(t, e.Store.UpsertEvent(refA, model.NormalizedEvent{
		ID: "far", ICalUID: "f@x", StartUTC: time.Date(2026, 7, 5, 10, 0, 1, 0, time.UTC),
		EndUTC: time.Date(2026, 7, 5, 11, 0, 1, 0, time.UTC), IsBusy: true,
	}))
	e.checkReminders(ctx)
	require.Empty(t, fn.reminders)
}

func TestCheckRemindersDedupesSameMeeting(t *testing.T) {
	e, fn, _ := reminderEngine(t)
	ctx := context.Background()
	start := time.Date(2026, 7, 5, 9, 55, 0, 0, time.UTC)
	refB := model.CalendarRef{AccountID: "b", CalendarID: "primary"}
	require.NoError(t, e.Store.UpsertEvent(refA, model.NormalizedEvent{
		ID: "ev-a", ICalUID: "same@x", Title: "会議", StartUTC: start, EndUTC: start.Add(time.Hour), IsBusy: true,
	}))
	require.NoError(t, e.Store.UpsertEvent(refB, model.NormalizedEvent{
		ID: "ev-b", ICalUID: "same@x", Title: "会議", StartUTC: start, EndUTC: start.Add(time.Hour), IsBusy: true,
	}))

	e.checkReminders(ctx)
	require.Len(t, fn.reminders, 1) // 1 通のみ

	// スキップした側も記録される(記録条件 (b)。スペック 6 章)
	sentA, err := e.Store.WasReminderSent(refA, "ev-a", start)
	require.NoError(t, err)
	sentB, err := e.Store.WasReminderSent(refB, "ev-b", start)
	require.NoError(t, err)
	require.True(t, sentA && sentB)
}

func TestCheckRemindersRetryPolicy(t *testing.T) {
	e, fn, _ := reminderEngine(t)
	ctx := context.Background()
	start := time.Date(2026, 7, 5, 9, 55, 0, 0, time.UTC)
	require.NoError(t, e.Store.UpsertEvent(refA, model.NormalizedEvent{
		ID: "ev1", ICalUID: "u@x", StartUTC: start, EndUTC: start.Add(time.Hour), IsBusy: true,
	}))

	// リトライ可能エラー → 未記録 → 次回再送
	fn.failWith = errors.New("network down")
	e.checkReminders(ctx)
	require.Empty(t, fn.reminders)
	e.checkReminders(ctx)
	require.Len(t, fn.reminders, 1)

	// リトライ不能エラー → 記録してログ 1 回(スペック 6 章)
	start2 := time.Date(2026, 7, 5, 9, 56, 0, 0, time.UTC)
	require.NoError(t, e.Store.UpsertEvent(refA, model.NormalizedEvent{
		ID: "ev2", ICalUID: "v@x", StartUTC: start2, EndUTC: start2.Add(time.Hour), IsBusy: true,
	}))
	fn.failWith = fmt.Errorf("channel_not_found: %w", notify.ErrNonRetryable)
	e.checkReminders(ctx)
	require.Len(t, fn.reminders, 1) // 送れていない
	sent, err := e.Store.WasReminderSent(refA, "ev2", start2)
	require.NoError(t, err)
	require.True(t, sent) // だが記録されている(以後試行しない)
}

func TestCheckRemindersNoopWhenDisabled(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Notifier = nil
	e.checkReminders(context.Background()) // panic しないこと
}

// Run は起動直後の初回 tick の後にもリマインド判定を行う(スペック 9 章の統合点 (4)。
// PollInterval を 1 時間にし、ループ内 tick を待たずに送られることで検証する)。
func TestRunChecksRemindersOnStartupTick(t *testing.T) {
	e, fn, _ := reminderEngine(t)
	e.Cfg.PollInterval = time.Hour
	start := time.Date(2026, 7, 5, 9, 55, 0, 0, time.UTC) // now(9:50)+5分 → ウィンドウ内
	require.NoError(t, e.Store.UpsertEvent(refA, model.NormalizedEvent{
		ID: "ev1", ICalUID: "u@x", Title: "会議",
		StartUTC: start, EndUTC: start.Add(time.Hour), IsBusy: true,
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	require.Eventually(t, func() bool {
		fn.mu.Lock()
		defer fn.mu.Unlock()
		return len(fn.reminders) == 1
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/engine/ -race -count=1 -run 'CheckReminders'`
Expected: FAIL(コンパイルエラー)

- [ ] **Step 3: 実装**

(a) `internal/engine/scheduler.go`: `nextReconcileAt` を `nextDailyAt` にリネームし、コメントを更新(reconcile_at と morning_digest の両方の nextAt 方式で使う旨)。`scheduler_test.go` の `TestNextReconcileAt` と呼び出し 2 箇所も追従。Task 8 で入れた `notify.go` 内の参照も `nextDailyAt` に更新。

```go
// nextDailyAt は now と同じロケーションで、hhmm("15:04" 形式)が指す直近の
// 未来時刻を返す(reconcile_at / morning_digest 共用の nextAt 方式)。
// 当日の hhmm が now より未来ならその日、now 以前なら翌日。
// hhmm が不正な場合は既定の "04:00" にフォールバックする(config 側の検証が正、
// ここは防御的フォールバック)。
func nextDailyAt(now time.Time, hhmm string) time.Time {
```

(b) `Run` の変更(スペック 9 章の 4 点):

```go
func (e *Engine) Run(ctx context.Context) error {
	reauth := make(map[string]bool)
	failures := make(map[model.CalendarRef]int)
	next := nextDailyAt(e.now(), e.Cfg.ReconcileAt)
	var nextDigest time.Time
	if hhmm := e.digestAt(); hhmm != "" {
		nextDigest = nextDailyAt(e.now(), hhmm)
	}
	ticker := time.NewTicker(e.Cfg.PollInterval)
	defer ticker.Stop()

	// 起動直後に 1 ティック実行(初回同期をポーリング間隔まで待たせない)
	e.tick(ctx, reauth, failures)
	// 初回 tick の直後にもリマインド判定(再起動直後の通知を 1 tick 遅らせない。スペック 9 章)
	e.checkReminders(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		// ダイジェストは reconcile より先に判定する(同一 tick で両方期限到来時に
		// reconcile の所要時間でダイジェストを遅らせない。スペック 5 章)
		if !nextDigest.IsZero() && !e.now().Before(nextDigest) {
			nextDigest = e.runDigest(ctx, nextDigest)
		}
		if !e.now().Before(next) {
			if err := e.Reconcile(ctx); err != nil {
				log.Printf("reconcile: %v", err)
			}
			next = nextDailyAt(e.now(), e.Cfg.ReconcileAt)
			// 日次リコンサイルのタイミングで reauth スキップを解除し再試行する
			// (再認証済みなら次のティックから自動バックフィルされる)
			reauth = make(map[string]bool)
			failures = make(map[model.CalendarRef]int)
		}
		e.tick(ctx, reauth, failures)
		e.checkReminders(ctx)
	}
}
```

(c) `internal/engine/notify.go` に checkReminders を追加:

```go
// checkReminders は毎 tick(同期処理の後 — キャッシュが最新の状態で)、開始が
// リマインドウィンドウに入った未通知イベントへリマインドを送る(スペック 6 章)。
// 記録条件は (a) 送信成功時 (b) dedupe により送信不要と確定した時。
// リトライ可能エラーは未記録のまま次 tick で自然リトライされ、開始時刻を過ぎると
// 抽出条件から外れて自然に止まる。リトライ不能エラーは記録してログ 1 回に留める。
func (e *Engine) checkReminders(ctx context.Context) {
	if e.Notifier == nil {
		return
	}
	sc := e.Cfg.Notifications.Slack
	if sc == nil || sc.RemindBefore <= 0 {
		return
	}
	now := e.now()
	ups, err := e.Store.ListUpcomingEvents(now, sc.RemindBefore)
	if err != nil {
		log.Printf("reminders: %v", err)
		return
	}
	for _, u := range ups {
		if ctx.Err() != nil {
			return
		}
		sent, err := e.Store.WasReminderSent(u.Ref, u.EventID, u.StartUTC)
		if err != nil {
			log.Printf("reminder %s/%s: %v", u.Ref, u.EventID, err)
			continue
		}
		if sent {
			continue
		}
		if e.Cfg.DedupeSameMeeting {
			dup, err := e.Store.HasReminderForICalUID(u.ICalUID, u.StartUTC)
			if err != nil {
				log.Printf("reminder %s/%s: %v", u.Ref, u.EventID, err)
				continue
			}
			if dup {
				// 送信せず自分も記録する(以後の照会を単純化。スペック 6 章の記録条件 (b))
				if merr := e.Store.MarkReminderSent(u.Ref, u.EventID, u.ICalUID, u.StartUTC, now); merr != nil {
					log.Printf("reminder %s/%s: mark: %v", u.Ref, u.EventID, merr)
				}
				continue
			}
		}
		entry := DigestEntry{
			Title:      u.Title,
			StartUTC:   u.StartUTC,
			EndUTC:     u.EndUTC,
			AccountIDs: []string{u.Ref.AccountID},
		}
		if err := e.Notifier.SendReminder(ctx, entry, u.StartUTC.Sub(now)); err != nil {
			if errors.Is(err, notify.ErrNonRetryable) {
				log.Printf("reminder %s/%s: %v (non-retryable; giving up)", u.Ref, u.EventID, err)
				if merr := e.Store.MarkReminderSent(u.Ref, u.EventID, u.ICalUID, u.StartUTC, now); merr != nil {
					log.Printf("reminder %s/%s: mark: %v", u.Ref, u.EventID, merr)
				}
				continue
			}
			log.Printf("reminder %s/%s: %v (retrying next tick)", u.Ref, u.EventID, err)
			continue
		}
		if merr := e.Store.MarkReminderSent(u.Ref, u.EventID, u.ICalUID, u.StartUTC, now); merr != nil {
			log.Printf("reminder %s/%s: mark: %v", u.Ref, u.EventID, merr)
		}
	}
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/engine/ -race -count=1` → PASS
Run: `go test ./... -race -count=1` → PASS(リネームの取りこぼし検出)

- [ ] **Step 5: コミット**

```bash
git add internal/engine/
git commit -m "feat: fire start-of-event reminders and wire digest into the run loop"
```

---

### Task 10: cmd_run の配線(トークン検証+Notifier 注入)

**Files:**
- Modify: `cmd/calsync/cmd_run.go`
- Test: `cmd/calsync/cli_test.go`(追記)

**Interfaces:**
- Consumes: `config.Config.Notifications.Slack`(Task 4)、`slack.New`(Task 7)、`engine.Engine.Notifier`(Task 6)

- [ ] **Step 1: 失敗するテストを書く**

`cmd/calsync/cli_test.go` に追記(既存のコマンド実行ヘルパーがあればそれに合わせる):

```go
// Slack 設定があるのにトークン環境変数が空なら、store を開く前に fail fast する
// (スペック 9 章。status/doctor は環境変数なしで動くため run のみで検証)。
func TestRunFailsFastWhenSlackTokenMissing(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "calsync.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
notifications:
  slack:
    bot_token_env: CALSYNC_TEST_SLACK_TOKEN
    channel: "C123"
    morning_digest: "07:30"
accounts:
  - id: a
    provider: google
`), 0o600))
	t.Setenv("CALSYNC_TEST_SLACK_TOKEN", "")
	rootCmd.SetArgs([]string{"run", "--config", cfgPath, "--data", t.TempDir()})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	err := rootCmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "CALSYNC_TEST_SLACK_TOKEN")
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./cmd/calsync/ -race -count=1 -run TestRunFailsFast`
Expected: FAIL(現状はトークン未検証のまま buildEngine に進み、トークンファイル不在のエラーになる — エラーメッセージに環境変数名が含まれず require.Contains が落ちる)

- [ ] **Step 3: 実装**

`cmd/calsync/cmd_run.go` を以下に変更(import に `fmt` / `os` / `engine` / `slack` を追加):

```go
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/engine"
	"github.com/btajp/calsync/internal/notify/slack"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the sync daemon (polling loop + daily reconcile)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(flagConfig)
		if err != nil {
			return err
		}
		// Slack 通知が設定されていればトークンを先に検証する(store を開く前に
		// fail fast。デーモン専用機能のため run でのみ検証する。スペック 9 章)
		var notifier engine.Notifier
		if sc := cfg.Notifications.Slack; sc != nil {
			token := os.Getenv(sc.BotTokenEnv)
			if token == "" {
				return fmt.Errorf("notifications.slack: environment variable %s is not set (export the bot token or remove the notifications section)", sc.BotTokenEnv)
			}
			notifier = slack.New(token, sc.Channel)
		}
		eng, err := buildEngine(cfg, flagData)
		if err != nil {
			return err
		}
		defer eng.Store.Close()
		eng.Notifier = notifier
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return eng.Run(ctx)
	},
}

func init() { rootCmd.AddCommand(runCmd) }
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./cmd/calsync/ -race -count=1`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add cmd/calsync/
git commit -m "feat: wire Slack notifier into calsync run with fail-fast token check"
```

---

### Task 11: 掃除フック(Reconcile 48h+RemoveAccount)

**Files:**
- Modify: `internal/engine/reconcile.go`(`Reconcile` 末尾)
- Modify: `internal/engine/engine.go`(`RemoveAccount` ステップ 3)
- Test: `internal/engine/reconcile_test.go`、`internal/engine/remove_test.go`(各追記)

**Interfaces:**
- Consumes: `Store.CleanupRemindersSent` / `Store.DeleteRemindersForAccount`(Task 5)

- [ ] **Step 1: 失敗するテストを書く**

(a) `internal/engine/reconcile_test.go` に追記:

```go
// 日次リコンサイルが reminders_sent の古い行(start_utc < now-48h)を掃除する(スペック 4.3)。
func TestReconcileCleansOldReminderRecords(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()
	now := e.now()
	old := now.Add(-72 * time.Hour)
	recent := now.Add(-time.Hour)
	require.NoError(t, e.Store.MarkReminderSent(refA, "old", "o@x", old, old))
	require.NoError(t, e.Store.MarkReminderSent(refA, "recent", "r@x", recent, recent))

	require.NoError(t, e.Reconcile(ctx))

	sent, err := e.Store.WasReminderSent(refA, "old", old)
	require.NoError(t, err)
	require.False(t, sent)
	sent, err = e.Store.WasReminderSent(refA, "recent", recent)
	require.NoError(t, err)
	require.True(t, sent)
}
```

(b) `internal/engine/remove_test.go` に追記:

```go
// accounts remove がそのアカウントの reminders_sent も削除する(スペック 4.3)。
func TestRemoveAccountDeletesReminderRecords(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()
	start := e.now()
	require.NoError(t, e.Store.MarkReminderSent(refA, "ev1", "u@x", start, start))

	require.NoError(t, RemoveAccount(ctx, e, stubTokenDeleter{}, "a", true))

	sent, err := e.Store.WasReminderSent(refA, "ev1", start)
	require.NoError(t, err)
	require.False(t, sent)
}
```

※ `stubTokenDeleter` は `remove_test.go` に既存のトークン削除スタブがあればそれを使う(名前が違う場合は既存に合わせる。無ければ `type stubTokenDeleter struct{}` + `func (stubTokenDeleter) Delete(string) error { return nil }` を追加)。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/engine/ -race -count=1 -run 'ReminderRecords'`
Expected: FAIL(掃除されず True のまま)

- [ ] **Step 3: 実装**

(a) `internal/engine/reconcile.go` の `Reconcile`、`reevaluateSuppressed` ブロックの後・最終 `return errors.Join(errs...)` の直前に:

```go
	// 通知送信記録の掃除(スペック 4.3: start_utc < now-48h。日次リコンサイルは
	// デーモンで常に有効なため、ここへの相乗りでテーブルは肥大しない)
	if err := e.Store.CleanupRemindersSent(e.now().Add(-48 * time.Hour)); err != nil {
		errs = append(errs, fmt.Errorf("cleanup reminders_sent: %w", err))
	}
```

(b) `internal/engine/engine.go` の `RemoveAccount` ステップ (3)、`DeleteCalendarsForAccount` の後・`tokens.Delete` の前に:

```go
	if err := e.Store.DeleteRemindersForAccount(accountID); err != nil {
		return err
	}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/engine/ -race -count=1`
Expected: PASS

- [ ] **Step 5: コミット**

```bash
git add internal/engine/
git commit -m "feat: clean up reminder records on reconcile and account removal"
```

---

### Task 12: ドキュメント+最終検証

**Files:**
- Modify: `README.md`(Slack 通知セクション追加)
- Modify: `CHANGELOG.md`(`[Unreleased]` の `### Added`)
- Modify: `.agents/skills/calsync-setup/SKILL.md`(Slack セットアップ節)
- Modify: `docs/superpowers/specs/2026-07-05-slack-notifications-design.md`(スパイク消し込み)

- [ ] **Step 1: README に Slack 通知セクションを追加**

設定例・Slack App 作成手順(api.slack.com/apps → Create New App → OAuth & Permissions → Bot Token Scopes: `chat:write`、DM 用 `im:write`、未参加公開チャンネル用 `chat:write.public` → Install to Workspace → `xoxb-` トークン取得)・compose での環境変数の渡し方(`environment: - SLACK_BOT_TOKEN=${SLACK_BOT_TOKEN}` と `.env`)・プライベートチャンネルには Bot の招待(`/invite @app名`)が必要なこと・**通知先チャンネルの公開範囲への注意**(予定の件名が流れる)・制約(free/終日はリマインド対象外、停止中のダイジェストはキャッチアップなし)を記載する。既存 README の章立て・トーンに合わせること。

- [ ] **Step 2: CHANGELOG の `[Unreleased]` `### Added` 先頭に追記**

```markdown
- **Slack 通知(#10)**: 朝のダイジェスト(指定時刻に当日の実予定を全アカウント横断で通知。ライブ取得のため free の予定も件名付きで含む)と開始前リマインド(指定時間前に通知。イベントキャッシュ+送信記録テーブルで再起動しても二重送信しない)。`notifications.slack`(`bot_token_env` / `channel` / `morning_digest` / `remind_before`)で設定し、トークンは環境変数のみ。件名は Slack 仕様のエスケープ済み(メンションインジェクション防止)
```

- [ ] **Step 3: calsync-setup スキルに Slack 節を追加**

`.agents/skills/calsync-setup/SKILL.md` の既存構成に合わせ、「Slack 通知のセットアップ」節(App 作成 → スコープ → インストール → トークンを .env へ → channel ID の調べ方(チャンネル詳細の最下部 / DM はプロフィールのメンバー ID)→ プライベートチャンネルは Bot 招待必須)を追加する。

- [ ] **Step 4: スペックのスパイク消し込み**

`docs/superpowers/specs/2026-07-05-slack-notifications-design.md` 13 章:
- スパイク 2 を取り消し線+「コード確認済み(2026-07-05): 現行 Google 実装は `fields` でフィールドを絞っておらず summary は応答に含まれる。応答パースはユニットで検証済み」に更新
- スパイク 1・3・4 は「ユニット(フィクスチャ/httptest)検証済み。実 API は初回稼働時に要実測」の注記を追加して残す

- [ ] **Step 5: 最終検証一式**

```bash
go build -o ./calsync ./cmd/calsync
go test ./... -race -count=1
go vet ./...
gofmt -l internal/ cmd/        # 出力なしが正
docker compose config -q
```

Expected: すべて成功、gofmt は無出力。

- [ ] **Step 6: コミット**

```bash
git add README.md CHANGELOG.md .agents/skills/calsync-setup/SKILL.md docs/superpowers/specs/2026-07-05-slack-notifications-design.md
git commit -m "docs: document Slack notifications setup and mark resolved spikes"
```

---

## Plan Self-Review メモ

- スペック 3〜9 章の各要件はタスク 4(設定)・2/5(スキーマ)・8(ダイジェスト)・9(リマインド/ループ)・7(送信/エスケープ)・10(配線)・11(掃除)・12(ドキュメント)が対応。10 章のテスト計画は各タスクの Step 1 に分散して網羅
- 型シグネチャは Task 5・6 の Interfaces ブロックが正。後続タスクはこれを参照する
- Task 8 の `nextDailyAt` 参照は Task 9 のリネームまで `nextReconcileAt` を使う(タスク境界でコンパイルを保つための明示的な順序制約)
