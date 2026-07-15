# detail_sync(ペア別タイトル/説明同期)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `detail_sync` 設定で指定した origin => target アカウントペアに限り、ブロッカーのタイトル/説明を元イベントから転記し、内容変更も自動追従させる。

**Architecture:** 変更検知は mappings 保存ハッシュへの成分合成(既存 `policyHashFor` の "+desc" 方式の拡張)。ペア未設定なら保存ハッシュは従来値と完全一致(無風)。収容・再構築経路は無条件 sentinel で 1 回の自己修復 patch を保証。ペイロード組み立ては全経路共通の `blockerFor` に集約。

**Tech Stack:** Go(標準ライブラリ + 既存依存のみ。新規依存なし)、SQLite スキーマ変更なし。

**設計の正:** `docs/superpowers/specs/2026-07-15-detail-sync-design.md`(以下「スペック」)

## Global Constraints

- ビルド: `go build -o ./calsync ./cmd/calsync`(CGO 不要)
- テスト: `go test ./... -race -count=1`(必ず -race)
- 静的チェック: `go vet ./... && gofmt -l internal/ cmd/`(gofmt は出力なしが正)
- `go mod tidy` は禁止。新規依存を追加しない
- コミットは Conventional Commits(英語)。作業ブランチ `feat/detail-sync`
- **冪等キー(`model.GoogleBlockerID` / `model.MSTransactionID`)の導出は一切変更しない**(スペック §5)
- **`model.TimeHash` 自体は一切変更しない**(全レイヤー共有。成分はエンジン層で「後置合成」する)
- ループ防止・削除判定にタイトルを使わない(mappings 一次+タグ二次のまま)
- 既定(detail_sync 未設定)では通常同期経路の動作・保存ハッシュとも現状と完全同一であること(スペック §1/§3 の無風保証)

---

### Task 1: model.DetailHash(内容成分ハッシュ)

**Files:**
- Modify: `internal/model/model.go`(`TimeHash` の直後に追加。import に `strings` を追加)
- Test: `internal/model/model_test.go`

**Interfaces:**
- Consumes: なし(標準ライブラリのみ)
- Produces: `model.DetailHash(syncTitle, syncDescription bool, title, description string) string` — 16 桁 hex。Task 3 のエンジンが `"+detail:" + DetailHash(...)` の形で使う

- [ ] **Step 1: ブランチ作成**

```bash
git checkout -b feat/detail-sync
```

- [ ] **Step 2: 失敗するテストを書く**

`internal/model/model_test.go` の末尾に追加:

```go
// DetailHash は detail_sync ペアの内容変更検出用成分(スペック 2026-07-15 §3)。
func TestDetailHash(t *testing.T) {
	base := DetailHash(true, true, "A", "B")
	require.Len(t, base, 16)
	require.Equal(t, base, DetailHash(true, true, "A", "B"), "決定的であること")

	// 有効フィールドの内容変更で変化する
	require.NotEqual(t, base, DetailHash(true, true, "A2", "B"))
	require.NotEqual(t, base, DetailHash(true, true, "A", "B2"))

	// フィールド構成(fields)の変更で変化する(設定トグルの遡及反映のトリガー)
	require.NotEqual(t, DetailHash(true, false, "A", ""), DetailHash(true, true, "A", ""))

	// 無効フィールドの内容は影響しない(fields=[title] なら description 変更は不変)
	require.Equal(t, DetailHash(true, false, "A", "B"), DetailHash(true, false, "A", "C"))

	// 境界衝突耐性(スペック §3): 値を生連結すると同一になる組が、異なるハッシュになる
	require.NotEqual(t,
		DetailHash(true, true, "A|description|B", ""),
		DetailHash(true, true, "A", "B"))

	// 空タイトルは「空文字ベース」の値になる(表示側フォールバック値ではない — スペック §4。
	// events キャッシュ加温後にハッシュ不一致 → 自動修復が成立するための前提)
	require.NotEqual(t, DetailHash(true, false, "", ""), DetailHash(true, false, "予定あり", ""))
}
```

- [ ] **Step 3: テストが失敗することを確認**

Run: `go test ./internal/model/ -race -count=1 -run TestDetailHash`
Expected: FAIL(`undefined: DetailHash`)

- [ ] **Step 4: 実装**

`internal/model/model.go` の import に `"strings"` を追加し、`TimeHash` 関数の直後に追加:

```go
// DetailHash は detail_sync ペアの内容変更検出用成分(16桁hex。スペック 2026-07-15 §3)。
// 有効フィールドを正規順(title → description)で "<name>=<sha256hex(value)>" に写して
// "|" 連結した文字列をハッシュする。値を個別に sha256(64桁hex 全体)してから連結する
// のは、タイトルに "|description|" のような区切り文字列が含まれても隣接フィールドと
// 境界衝突しないため(TimeHash の "|" 連結は値が固定形式の時刻なので安全だが、
// ここは自由文字列が載る)。
func DetailHash(syncTitle, syncDescription bool, title, description string) string {
	var parts []string
	if syncTitle {
		sum := sha256.Sum256([]byte(title))
		parts = append(parts, "title="+hex.EncodeToString(sum[:]))
	}
	if syncDescription {
		sum := sha256.Sum256([]byte(description))
		parts = append(parts, "description="+hex.EncodeToString(sum[:]))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:16]
}
```

- [ ] **Step 5: テストが通ることを確認**

Run: `go test ./internal/model/ -race -count=1`
Expected: PASS(既存の TestTimeHash 系も含め全部)

- [ ] **Step 6: コミット**

```bash
git add internal/model/model.go internal/model/model_test.go
git commit -m "feat: add model.DetailHash for detail_sync content change detection"
```

---

### Task 2: config — detail_sync スキーマ・検証・DetailSyncFor

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: なし
- Produces:
  - `type DetailSyncPair struct { From, To string; Title, Description bool }`
  - `Config.DetailSync []DetailSyncPair`
  - `(*Config).DetailSyncFor(originID, targetID string) *DetailSyncPair`(無ければ nil)— Task 3 が使う

- [ ] **Step 1: 失敗するテストを書く**

`internal/config/config_test.go` の `TestLoad` のテーブルにケースを追加(`unknown notification keys are rejected` の直後):

```go
		{
			name: "detail_sync pairs are parsed and normalized",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title, description]
  - from: b
    to: a
    fields: [title]
`,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, []DetailSyncPair{
					{From: "a", To: "b", Title: true, Description: true},
					{From: "b", To: "a", Title: true},
				}, c.DetailSync)
			},
		},
		{
			name: "detail_sync unknown account is rejected",
			yaml: minimalYAML + `
detail_sync:
  - from: typo
    to: personal
    fields: [title]
`,
			wantErr: `detail_sync[0]: unknown account "typo"`,
		},
		{
			name: "detail_sync from==to is rejected",
			yaml: minimalYAML + `
detail_sync:
  - from: personal
    to: personal
    fields: [title]
`,
			wantErr: "from and to must differ",
		},
		{
			name: "detail_sync duplicate pair is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title]
  - from: a
    to: b
    fields: [description]
`,
			wantErr: `duplicate pair "a" => "b"`,
		},
		{
			name: "detail_sync invalid field is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [location]
`,
			wantErr: `invalid field "location" (want title or description)`,
		},
		{
			name: "detail_sync empty fields is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: []
`,
			wantErr: "fields must not be empty",
		},
		{
			name: "detail_sync duplicate field is rejected",
			yaml: `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title, title]
`,
			wantErr: `duplicate field "title"`,
		},
		{
			name: "detail_sync unknown key is rejected by KnownFields",
			yaml: minimalYAML + `
detail_sync:
  - form: personal
    to: personal
    fields: [title]
`,
			wantErr: "field form not found",
		},
```

さらにファイル末尾に追加:

```go
func TestDetailSyncFor(t *testing.T) {
	src := `
accounts:
  - id: a
    provider: google
    email: a@gmail.com
  - id: b
    provider: google
    email: b@gmail.com
detail_sync:
  - from: a
    to: b
    fields: [title]
`
	cfg, err := Load(writeConfig(t, src))
	require.NoError(t, err)

	p := cfg.DetailSyncFor("a", "b")
	require.NotNil(t, p)
	require.True(t, p.Title)
	require.False(t, p.Description)

	require.Nil(t, cfg.DetailSyncFor("b", "a"), "方向は一方通行(逆方向は別エントリ)")
	require.Nil(t, cfg.DetailSyncFor("a", "missing"))
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/config/ -race -count=1`
Expected: FAIL(`undefined: DetailSyncPair` 等のコンパイルエラー)

- [ ] **Step 3: 実装**

(1) `Config` struct(config.go:16-27)の `Accounts []Account` の直後にフィールド追加:

```go
	DetailSync        []DetailSyncPair
```

(2) `Account` struct 定義の直後に型を追加:

```go
// DetailSyncPair は detail_sync の 1 エントリ(スペック 2026-07-15 §2)。
// origin(From)→ target(To)アカウントの一方通行のペアで、指定したペアに限り
// ブロッカーのタイトル/説明を元イベントから転記する(既定は完全匿名のまま)。
// fields は検証時に bool へ正規化する(正規順 title → description はハッシュ側で担保)。
type DetailSyncPair struct {
	From, To           string
	Title, Description bool
}
```

(3) `rawConfig` に追加(`Accounts []rawAccount` の直後):

```go
	DetailSync        []rawDetailSync  `yaml:"detail_sync"`
```

(4) `rawAccount` の直後に raw 型を追加:

```go
type rawDetailSync struct {
	From   string   `yaml:"from"`
	To     string   `yaml:"to"`
	Fields []string `yaml:"fields"`
}
```

(5) `Load` のアカウントループ(`cfg.Accounts = append(cfg.Accounts, a)` で終わる for 文)の**直後**、`return cfg, nil` の前に検証を追加。`seen`(アカウント id の map)はループ後もスコープに残っているのでそのまま使う:

```go
	// detail_sync の検証(スペック 2026-07-15 §2)。アカウント id の実在チェックが
	// 必要なため、アカウントを全件読み終えた後の後検証パスで行う。
	seenPair := make(map[string]bool, len(raw.DetailSync))
	for i, rd := range raw.DetailSync {
		for _, id := range []string{rd.From, rd.To} {
			if !seen[id] {
				return nil, fmt.Errorf("config: detail_sync[%d]: unknown account %q", i, id)
			}
		}
		if rd.From == rd.To {
			return nil, fmt.Errorf("config: detail_sync[%d]: from and to must differ (got %q)", i, rd.From)
		}
		// アカウント id は ":" を含まない(上で検証済み)ため区切りに使える
		key := rd.From + ":" + rd.To
		if seenPair[key] {
			return nil, fmt.Errorf("config: detail_sync[%d]: duplicate pair %q => %q", i, rd.From, rd.To)
		}
		seenPair[key] = true
		if len(rd.Fields) == 0 {
			return nil, fmt.Errorf("config: detail_sync[%d]: fields must not be empty", i)
		}
		p := DetailSyncPair{From: rd.From, To: rd.To}
		for _, fld := range rd.Fields {
			switch fld {
			case "title":
				if p.Title {
					return nil, fmt.Errorf("config: detail_sync[%d]: duplicate field %q", i, fld)
				}
				p.Title = true
			case "description":
				if p.Description {
					return nil, fmt.Errorf("config: detail_sync[%d]: duplicate field %q", i, fld)
				}
				p.Description = true
			default:
				return nil, fmt.Errorf("config: detail_sync[%d]: invalid field %q (want title or description)", i, fld)
			}
		}
		cfg.DetailSync = append(cfg.DetailSync, p)
	}
```

(6) ファイル末尾(`AccountByID` の直後)にアクセサを追加:

```go
// DetailSyncFor は (origin, target) ペアの detail_sync エントリを返す。無ければ nil。
// 方向は一方通行(from => to)で、逆方向は別エントリ。
func (c *Config) DetailSyncFor(originID, targetID string) *DetailSyncPair {
	for i := range c.DetailSync {
		if c.DetailSync[i].From == originID && c.DetailSync[i].To == targetID {
			return &c.DetailSync[i]
		}
	}
	return nil
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/config/ -race -count=1`
Expected: PASS(既存ケース含め全部)

- [ ] **Step 5: コミット**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add detail_sync config schema, validation, and lookup"
```

---

### Task 3: engine — ハッシュ合成とペイロード転記

**Files:**
- Modify: `internal/engine/engine.go`(`upsertBlockerOnTarget` / `policyHashFor` 付近 / `blockerFor`)
- Modify: `internal/engine/dedupe.go`(`createFromMapping`)
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `model.DetailHash`(Task 1)、`Cfg.DetailSyncFor` / `config.DetailSyncPair`(Task 2)
- Produces: `(*Engine).detailComponentFor(originID, targetID string, ev model.NormalizedEvent) string` — `""` または `"+detail:<16hex>"`。Task 4 の sentinel 定数と対になる

- [ ] **Step 1: 失敗するテストを書く**

`internal/engine/engine_test.go` の末尾に追加(`enableOriginDescription` の流儀に合わせる):

```go
// ---- detail_sync: ペア別タイトル/説明同期(スペック 2026-07-15) ----

// enableDetailSync はテスト用に (from, to) ペアの detail_sync を設定する。
func enableDetailSync(e *Engine, from, to string, title, description bool) {
	e.Cfg.DetailSync = append(e.Cfg.DetailSync,
		config.DetailSyncPair{From: from, To: to, Title: title, Description: description})
}

func detailEvent(id string) model.NormalizedEvent {
	ev := busyEvent(id)
	ev.Title = "経営会議"
	ev.Description = "資料: https://example.com/doc"
	return ev
}

// ペア設定した c だけタイトル/説明が転記され、未設定の b は従来どおり(無風保証込み)
func TestUpsertBlockers_DetailSyncPerPair(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSync(e, "a", "c", true, true) // a => c のみ

	ev := detailEvent("ev1")
	require.NoError(t, e.processEvent(ctx, refA, ev))

	// c: タイトル・説明とも転記される
	blksC := f.Blockers(calCv)
	require.Len(t, blksC, 1)
	bc, ok := f.StoredBlocker(calCv, blksC[0].EventID)
	require.True(t, ok)
	require.Equal(t, "経営会議", bc.Title)
	require.Equal(t, "資料: https://example.com/doc", bc.Description)

	// b: 従来どおり固定タイトル・説明なし(完全匿名)
	blksB := f.Blockers(calBv)
	require.Len(t, blksB, 1)
	bb, ok := f.StoredBlocker(calBv, blksB[0].EventID)
	require.True(t, ok)
	require.Equal(t, "予定あり", bb.Title)
	require.Empty(t, bb.Description)

	// ハッシュ: c は "+detail:" 成分入り、b は素の TimeHash(無風保証の回帰テスト)
	mc, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)
	wantC := model.TimeHash(ev) + "+detail:" + model.DetailHash(true, true, ev.Title, ev.Description)
	require.Equal(t, wantC, mc.TimeHash)
	mb, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t, model.TimeHash(ev), mb.TimeHash, "ペア未設定の保存ハッシュは従来と完全同一")
}

// origin のタイトルだけ変わった(時刻不変)とき、ペア先のブロッカーが次の処理で patch される
func TestProcessEvent_DetailTitleChangePropagates(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSync(e, "a", "c", true, false)

	ev := detailEvent("ev1")
	require.NoError(t, e.processEvent(ctx, refA, ev))
	origID := f.Blockers(calCv)[0].EventID

	renamed := ev
	renamed.Title = "経営会議(リスケ)"
	require.NoError(t, e.processEvent(ctx, refA, renamed))

	blks := f.Blockers(calCv)
	require.Len(t, blks, 1)
	require.Equal(t, origID, blks[0].EventID, "patch であって再作成ではない")
	bc, ok := f.StoredBlocker(calCv, origID)
	require.True(t, ok)
	require.Equal(t, "経営会議(リスケ)", bc.Title)

	// fields=[title] なので description の変更ではハッシュが変わらない(=呼び出しなし)
	descOnly := renamed
	descOnly.Description = "別の本文"
	f.SeedBlocker(calCv, model.BlockerRecord{
		EventID: origID, OriginTag: blks[0].OriginTag, TimeHash: "tampered",
	})
	require.NoError(t, e.processEvent(ctx, refA, descOnly))
	after := f.Blockers(calCv)
	require.Equal(t, "tampered", after[0].TimeHash, "無効フィールドの変更ではプロバイダを呼ばない")
}

// 元タイトルが空のときは blocker_title へフォールバックする(スペック §4)
func TestBlockerFor_EmptyTitleFallsBackToBlockerTitle(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSync(e, "a", "c", true, false)

	ev := busyEvent("ev1") // Title は空
	require.NoError(t, e.processEvent(ctx, refA, ev))

	bc, ok := f.StoredBlocker(calCv, f.Blockers(calCv)[0].EventID)
	require.True(t, ok)
	require.Equal(t, "予定あり", bc.Title)

	// ハッシュは空文字ベースの detail 成分入り(素の TimeHash とは異なる。
	// キャッシュ加温で実タイトルが入ればハッシュ不一致 → 自動修復される前提)
	mc, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)
	require.Equal(t, model.TimeHash(ev)+"+detail:"+model.DetailHash(true, false, "", ""), mc.TimeHash)
}

// description 転記と show_origin_in_description の併記(スペック §4)
func TestUpsertBlockers_DetailDescriptionWithOriginLine(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSync(e, "a", "c", false, true)
	enableOriginDescription(e, "c", true)

	ev := detailEvent("ev1")
	require.NoError(t, e.processEvent(ctx, refA, ev))
	bc, ok := f.StoredBlocker(calCv, f.Blockers(calCv)[0].EventID)
	require.True(t, ok)
	require.Equal(t, "資料: https://example.com/doc\n\ncalsync: ミラー元アカウント = a", bc.Description)
	require.Equal(t, "予定あり", bc.Title, "fields=[description] ではタイトルは転記しない")

	// "+desc"(show_origin)の後に "+detail:" が付く固定順(スペック §3 の合成順)
	mc, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)
	require.Equal(t,
		model.TimeHash(ev)+"+desc"+"+detail:"+model.DetailHash(false, true, ev.Title, ev.Description),
		mc.TimeHash)

	// 元説明が空なら origin 行のみ
	empty := busyEvent("ev2")
	require.NoError(t, e.processEvent(ctx, refA, empty))
	var ev2Blocker string
	for _, rec := range f.Blockers(calCv) {
		if rec.OriginTag == model.OriginTagOf("a", "ev2") {
			ev2Blocker = rec.EventID
		}
	}
	bc2, ok := f.StoredBlocker(calCv, ev2Blocker)
	require.True(t, ok)
	require.Equal(t, "calsync: ミラー元アカウント = a", bc2.Description)
}

// 設定トグルの遡及反映(FullResync 経由)と、冪等キー・件数の不変(スペック §5 の中核主張)
func TestFullResync_DetailSyncTogglesRetroactively(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()

	// ペア未設定で作成(既存運用状態)
	ev := detailEvent("ev1")
	f.SetFullState(refA, []model.NormalizedEvent{ev})
	require.NoError(t, e.SyncCalendar(ctx, refA))
	blks := f.Blockers(calCv)
	require.Len(t, blks, 1)
	b0, _ := f.StoredBlocker(calCv, blks[0].EventID)
	require.Equal(t, "予定あり", b0.Title)
	m0, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)

	// ペア ON → リコンサイル(FullResync)で既存ブロッカーに遡及反映
	enableDetailSync(e, "a", "c", true, true)
	require.NoError(t, e.FullResync(ctx, refA))
	b1, ok := f.StoredBlocker(calCv, blks[0].EventID)
	require.True(t, ok)
	require.Equal(t, "経営会議", b1.Title)
	require.Equal(t, "資料: https://example.com/doc", b1.Description)

	// ペア OFF → 既定内容へ復帰(タイトル・説明とも)
	e.Cfg.DetailSync = nil
	require.NoError(t, e.FullResync(ctx, refA))
	b2, ok := f.StoredBlocker(calCv, blks[0].EventID)
	require.True(t, ok)
	require.Equal(t, "予定あり", b2.Title)
	require.Empty(t, b2.Description)

	// 冪等キーとブロッカー件数はトグル前後で不変(二重作成しない — スペック §5)
	m2, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)
	require.Equal(t, m0.IdempotencyKey, m2.IdempotencyKey)
	require.Equal(t, blks[0].EventID, m2.BlockerEventID)
	require.Len(t, f.Blockers(calCv), 1)
	require.Equal(t, model.TimeHash(ev), m2.TimeHash, "OFF に戻せば保存ハッシュも従来値へ戻る")
}

// suppressed 昇格時も転記内容で作成される(全経路共通ヘルパの検証 — スペック §4)
func TestPromoteSuppressed_UsesDetailSyncContent(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSync(e, "a", "c", true, false)

	ev := detailEvent("ev1")
	// c に同一会議の実予定(同 iCalUID・同開始)を置く → c への配布は suppressed になる
	dup := ev
	dup.ID = "dup1"
	require.NoError(t, e.Store.UpsertCalendar(calCv))
	require.NoError(t, e.Store.UpsertEvent(calCv, dup))
	require.NoError(t, e.processEvent(ctx, refA, ev))
	m, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)
	require.Equal(t, store.StatusSuppressed, m.Status)

	// 実予定が消えた → 昇格。作成されるブロッカーは転記タイトル
	require.NoError(t, e.Store.DeleteEvent(calCv, "dup1"))
	require.NoError(t, e.promoteSuppressed(ctx, "c", ev.ICalUID))
	blks := f.Blockers(calCv)
	require.Len(t, blks, 1)
	bc, ok := f.StoredBlocker(calCv, blks[0].EventID)
	require.True(t, ok)
	require.Equal(t, "経営会議", bc.Title)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/engine/ -race -count=1 -run 'DetailSync|DetailTitle|EmptyTitleFalls|DetailDescription|PromoteSuppressed_Uses'`
Expected: FAIL(転記されず "予定あり" のまま・ハッシュ不一致)

- [ ] **Step 3: engine.go の実装**

(1) `policyHashFor` の直後に追加:

```go
// detailComponentFor は detail_sync ペアの内容成分("+detail:<16hex>")を返す。
// ペア未設定なら空文字 = 保存ハッシュは従来値と完全一致(スペック 2026-07-15 §3 の
// 無風保証)。フラグではなく内容そのものをハッシュに入れるため、origin のタイトル/
// 説明変更が次の差分同期でハッシュ不一致 → patch として伝搬する。
// 合成は必ず policyHashFor の出力の末尾に行う(合成順が経路間でブレると永久不一致になる)。
func (e *Engine) detailComponentFor(originID, targetID string, ev model.NormalizedEvent) string {
	p := e.Cfg.DetailSyncFor(originID, targetID)
	if p == nil {
		return ""
	}
	return "+detail:" + model.DetailHash(p.Title, p.Description, ev.Title, ev.Description)
}
```

(2) `upsertBlockerOnTarget` の合成行を変更:

```go
	// description ポリシーを合成した変更検出ハッシュ(トグルの遡及反映用。Issue #7)
	timeHash = e.policyHashFor(timeHash, target.ID)
```
↓
```go
	// description ポリシー(Issue #7)と detail_sync 内容成分(スペック 2026-07-15 §3)を
	// 合成した変更検出ハッシュ。トグル・内容変更の両方が「不一致」として検出される
	timeHash = e.policyHashFor(timeHash, target.ID) + e.detailComponentFor(ref.AccountID, target.ID, ev)
```

(3) `blockerFor` を変更(タイトル/説明の決定を集約):

```go
func (e *Engine) blockerFor(ctx context.Context, ev model.NormalizedEvent, originTag string, targetCal model.CalendarRef, p provider.Provider) (model.Blocker, error) {
	tz := ""
	if ev.IsAllDay {
		var err error
		tz, err = e.targetTimezone(ctx, targetCal, p)
		if err != nil {
			return model.Blocker{}, err
		}
	}
	title := e.Cfg.BlockerTitle
	desc := e.descriptionFor(targetCal.AccountID, originTag)
	originID, _, _ := strings.Cut(originTag, ":")
	if pol := e.Cfg.DetailSyncFor(originID, targetCal.AccountID); pol != nil {
		// detail_sync ペア(スペック 2026-07-15 §4): タイトル/説明を元イベントから転記。
		// 空タイトルは blocker_title へフォールバック(events キャッシュの旧行が
		// 加温前でも壊れない。ハッシュには空文字ベースの成分が入るため加温後に自己修復)
		if pol.Title && ev.Title != "" {
			title = ev.Title
		}
		if pol.Description && ev.Description != "" {
			if desc != "" {
				desc = ev.Description + "\n\n" + desc // show_origin の origin 行は末尾に併記
			} else {
				desc = ev.Description
			}
		}
	}
	return model.Blocker{
		Title:          title,
		StartUTC:       ev.StartUTC,
		EndUTC:         ev.EndUTC,
		IsAllDay:       ev.IsAllDay,
		AllDayStart:    ev.AllDayStart,
		AllDayEnd:      ev.AllDayEnd,
		TargetTimezone: tz,
		OriginTag:      originTag,
		Description:    desc,
	}, nil
}
```

- [ ] **Step 4: dedupe.go の実装**

`createFromMapping` のハッシュ行を変更:

```go
	m.TimeHash = e.policyHashFor(model.TimeHash(ev), m.TargetAccount)
```
↓
```go
	m.TimeHash = e.policyHashFor(model.TimeHash(ev), m.TargetAccount) +
		e.detailComponentFor(m.OriginAccount, m.TargetAccount, ev)
```

- [ ] **Step 5: テストが通ることを確認**

Run: `go test ./internal/engine/ -race -count=1`
Expected: PASS(新規+既存全部。既存の `TestUpsertBlockers_OriginDescriptionPerTarget` 等はペア未設定なので影響しない)

- [ ] **Step 6: コミット**

```bash
git add internal/engine/engine.go internal/engine/dedupe.go internal/engine/engine_test.go
git commit -m "feat: per-pair title/description sync in blocker payload and change hash"
```

---

### Task 4: reconcile — 収容・再構築経路の無条件 sentinel

**Files:**
- Modify: `internal/engine/engine.go`(定数追加)
- Modify: `internal/engine/reconcile.go`(`rebuildMappingsFromTags` / `adoptOrphan` の 2 箇所)
- Test: `internal/engine/reconcile_test.go`(新規テスト+既存 1 件のアサーション更新)

**Interfaces:**
- Consumes: `detailComponentFor`(Task 3)
- Produces: `detailHashSentinel` 定数(engine パッケージ内部)

- [ ] **Step 1: 失敗するテストを書く**

`internal/engine/reconcile_test.go` の末尾に追加:

```go
// ---- detail_sync: 収容・再構築経路の sentinel(スペック 2026-07-15 §6) ----

// タグ再構築(フェーズ0)は sentinel 付きで収容し、同一リコンサイル内の FullResync
// 再処理で 1 回だけ patch されて正しい内容+正規ハッシュに自己修復する。
// sentinel はペア設定の有無に関わらず付与される(ペア解除 → DB 全損 → 再構築の
// 経路で転記内容が残留するプライバシー穴を塞ぐ)。
func TestReconcile_RebuildSelfHealsBlockerContent(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	ev := busyEvent("ev1")
	ev.Title = "経営会議"
	tag := model.OriginTagOf("a", "ev1")

	// DB 全損相当: mappings は空だが、b 上に「転記タイトル付き」ブロッカーが残っている
	// (ペア設定があった時期に作られた想定)。origin はフル同期で生きている
	f.SetFullState(refA, []model.NormalizedEvent{ev})
	f.SetFullState(calBv, nil)
	f.SetFullState(calCv, nil)
	blkID, err := f.CreateBlocker(ctx, calBv, model.Blocker{
		Title: "経営会議", StartUTC: ev.StartUTC, EndUTC: ev.EndUTC, OriginTag: tag,
	}, model.MSTransactionID(tag, "b"))
	require.NoError(t, err)

	// ペアは現在「未設定」= 復帰の期待値は既定内容
	require.NoError(t, e.Reconcile(ctx))

	// 転記タイトルが既定の「予定あり」へ復帰し、ハッシュも正規値(素の TimeHash)になる
	body, ok := f.StoredBlocker(calBv, blkID)
	require.True(t, ok)
	require.Equal(t, "予定あり", body.Title)
	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t, store.StatusActive, m.Status)
	require.Equal(t, model.TimeHash(ev), m.TimeHash, "修復後は sentinel が消えて正規ハッシュ")

	// 2 回目のリコンサイルでは patch が走らない(1 回限りの自己修復)
	f.SeedBlocker(calBv, model.BlockerRecord{EventID: blkID, OriginTag: tag, TimeHash: "tampered"})
	require.NoError(t, e.Reconcile(ctx))
	require.Equal(t, "tampered", f.Blockers(calBv)[0].TimeHash, "2 回目は呼び出しなし")
}

// ペア設定ありでも同様: 再構築 → FullResync で転記内容+正規ハッシュに収斂する
func TestReconcile_RebuildSelfHealsWithDetailSyncPair(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSync(e, "a", "b", true, false)
	ev := busyEvent("ev1")
	ev.Title = "経営会議"
	tag := model.OriginTagOf("a", "ev1")

	f.SetFullState(refA, []model.NormalizedEvent{ev})
	f.SetFullState(calBv, nil)
	f.SetFullState(calCv, nil)
	// 古い既定タイトルのままのブロッカーが残っている(ペア設定前に作られた想定)
	blkID, err := f.CreateBlocker(ctx, calBv, model.Blocker{
		Title: "予定あり", StartUTC: ev.StartUTC, EndUTC: ev.EndUTC, OriginTag: tag,
	}, model.MSTransactionID(tag, "b"))
	require.NoError(t, err)

	require.NoError(t, e.Reconcile(ctx))

	body, ok := f.StoredBlocker(calBv, blkID)
	require.True(t, ok)
	require.Equal(t, "経営会議", body.Title, "再構築後の修復 patch で転記タイトルになる")
	m, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t,
		model.TimeHash(ev)+"+detail:"+model.DetailHash(true, false, ev.Title, ev.Description),
		m.TimeHash)
}
```

- [ ] **Step 2: 既存テストのアサーションを sentinel 仕様に更新**

`TestReconcile_AdoptsOrphanWhenOriginAlive`(reconcile_test.go:244 付近)は「origin のフル同期が失敗し、修復 patch が走らないままのハッシュ」を検証しているため、期待値が sentinel 付きに変わる:

```go
	require.Equal(t, model.TimeHash(ev), m.TimeHash)
```
↓
```go
	// 収容時は内容成分を再現できないため sentinel 付きで保存される(スペック 2026-07-15 §6)。
	// origin のフル同期が失敗しているので修復 patch は次回に持ち越される
	require.Equal(t, model.TimeHash(ev)+"+detail:unknown", m.TimeHash)
```

他に収容ハッシュを検証している既存テストがあれば同様に更新する(`grep -n 'policyHashFor\|TimeHash(ev), m.TimeHash' internal/engine/reconcile_test.go` で洗い出す)。

- [ ] **Step 3: テストが失敗することを確認**

Run: `go test ./internal/engine/ -race -count=1 -run 'Reconcile_RebuildSelfHeals|Reconcile_AdoptsOrphan'`
Expected: FAIL(sentinel 未実装のため新テスト・更新済みアサーションとも不一致)

- [ ] **Step 4: 実装**

(1) `internal/engine/engine.go` の `detailComponentFor` の直前に定数を追加:

```go
// detailHashSentinel は収容・再構築経路(rebuildMappingsFromTags / adoptOrphan)で
// 保存する内容成分の仮値(スペック 2026-07-15 §6)。ListBlockers はブロッカーの実時刻
// しか返せず内容成分を再現できないため、実ハッシュ(hex)とも素のハッシュとも決して
// 一致しないこの値を置き、直後の FullResync 再処理で必ず 1 回 patch を走らせて
// 正しい内容+正規ハッシュに自己修復させる。ペア未設定の行にも無条件で付与する
// (ペア解除 → 反映前に DB 全損 → 再構築、で転記内容が残留する穴を塞ぐ。
// 代償は再構築行への 1 回限りの patch のみ)。
const detailHashSentinel = "+detail:unknown"
```

(2) `internal/engine/reconcile.go` の `rebuildMappingsFromTags` 内:

```go
				TimeHash:       e.policyHashFor(rec.TimeHash, acct.ID),
```
↓
```go
				TimeHash:       e.policyHashFor(rec.TimeHash, acct.ID) + detailHashSentinel,
```

(3) 同ファイル `adoptOrphan` 内(同じ形の行):

```go
		TimeHash:       e.policyHashFor(rec.TimeHash, acct.ID),
```
↓
```go
		TimeHash:       e.policyHashFor(rec.TimeHash, acct.ID) + detailHashSentinel,
```

- [ ] **Step 5: テストが通ることを確認**

Run: `go test ./internal/engine/ -race -count=1`
Expected: PASS(全部。他の既存テストが落ちた場合、収容ハッシュの期待値だけを Step 2 の要領で更新する — 実装側を曲げない)

- [ ] **Step 6: コミット**

```bash
git add internal/engine/engine.go internal/engine/reconcile.go internal/engine/reconcile_test.go
git commit -m "feat: store sentinel detail component on adoption/rebuild for one-shot self-heal"
```

---

### Task 5: Google — UpdateBlocker に Summary、409 非 cancelled で内容整合 patch

**Files:**
- Modify: `internal/provider/google/blockers.go`(`UpdateBlocker` / `CreateBlocker`)
- Test: `internal/provider/google/blockers_test.go`(既存 2 件更新+アサーション追加)

**Interfaces:**
- Consumes: なし(provider 層内で完結)
- Produces: エンジンから見た契約変更 —「UpdateBlocker はタイトルも更新する」「CreateBlocker の 409 収容は内容を揃えてから ID を返す」。fake は既に同契約(`internal/provider/fake/fake.go` の UpdateBlocker は body 全置換)なので変更不要

- [ ] **Step 1: 既存テストを新契約に更新(失敗する状態を作る)**

(1) `TestUpdateBlockerPatchesTimesAndDescription`(blockers_test.go:219)を更新:

```go
	b := model.Blocker{
		Title:    "予定あり", // patch には含めない(時刻変更のみ)
		StartUTC: time.Date(2026, 7, 11, 3, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC),
	}
```
↓
```go
	b := model.Blocker{
		Title:    "経営会議", // detail_sync 対応で patch にも summary を含める(スペック 2026-07-15 §5)
		StartUTC: time.Date(2026, 7, 11, 3, 0, 0, 0, time.UTC),
		EndUTC:   time.Date(2026, 7, 11, 4, 0, 0, 0, time.UTC),
	}
```

同テストのキー検証とアサーションを更新:

```go
	require.ElementsMatch(t, []string{"start", "end", "description"}, mapKeys(m), "patch は start/end のみ")
```
↓
```go
	require.ElementsMatch(t, []string{"start", "end", "description", "summary"}, mapKeys(m),
		"patch は start/end/description/summary(detail_sync のタイトル追従)")
	require.Equal(t, "経営会議", m["summary"])
```

(2) `TestCreateBlockerConflictAdoptsExisting`(blockers_test.go:116)を更新 — ハンドラに PATCH 応答を追加し、期待リクエスト列を 3 件にする:

```go
	rec := &recorder{}
	handler := rec.wrap(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error": {"errors": [{"domain": "global", "reason": "duplicate",
				"message": "The requested identifier already exists."}],
				"code": 409, "message": "The requested identifier already exists."}}`)
			return
		}
		fmt.Fprintf(w, `{"id": %q, "status": "confirmed"}`, idem)
	})
```
(GET も PATCH も同じ成功応答で足りるためハンドラは変更不要。アサーション部のみ変更)

```go
	reqs := rec.all()
	require.Len(t, reqs, 2)
	require.Equal(t, http.MethodPost, reqs[0].Method)
	require.Equal(t, http.MethodGet, reqs[1].Method, "409 後は events.get で実在確認する")
	require.Equal(t, "/calendars/primary/events/"+idem, reqs[1].Path)
```
↓
```go
	reqs := rec.all()
	require.Len(t, reqs, 3)
	require.Equal(t, http.MethodPost, reqs[0].Method)
	require.Equal(t, http.MethodGet, reqs[1].Method, "409 後は events.get で実在確認する")
	require.Equal(t, "/calendars/primary/events/"+idem, reqs[1].Path)
	// クラッシュ再実行の収容では既存ブロッカーの内容が古い可能性があるため、
	// 作成しようとしていた内容で patch してから ID を返す(スペック 2026-07-15 §5)
	require.Equal(t, http.MethodPatch, reqs[2].Method)
	require.Equal(t, "/calendars/primary/events/"+idem, reqs[2].Path)
	pm := decodeBody(t, reqs[2].Body)
	require.Equal(t, "予定あり", pm["summary"])
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/provider/google/ -race -count=1 -run 'UpdateBlockerPatches|ConflictAdopts'`
Expected: FAIL(summary が patch に無い / リクエストが 2 件のまま)

- [ ] **Step 3: 実装**

(1) `UpdateBlocker` の patch に Summary を追加し、コメントを更新:

```go
// UpdateBlocker は events.patch で start/end のみ更新する(タイトル等は送らない)。
// 404(ブロッカーが手動削除等で消えている)は provider.ErrNotFound に写像し、
// エンジン側が「pending 化して再作成」にフォールバックできるようにする(仕様8章4)。
func (c *Client) UpdateBlocker(ctx context.Context, cal model.CalendarRef, eventID string, b model.Blocker) error {
	svc, err := c.service(ctx)
	if err != nil {
		return err
	}
	patch := &calendar.Event{
		Start:       blockerStart(b),
		End:         blockerEnd(b),
		Description: b.Description,
		// 空文字でのクリア(show_origin_in_description のトグル OFF)も
		// 反映するため、Description は常に明示送信する(Issue #7)
		ForceSendFields: []string{"Description"},
	}
```
↓
```go
// UpdateBlocker は events.patch で start/end/summary/description を更新する。
// summary は detail_sync のタイトル追従・復帰のため常に送る(エンジン側の
// フォールバックにより常に非空 — スペック 2026-07-15 §5)。
// 404(ブロッカーが手動削除等で消えている)は provider.ErrNotFound に写像し、
// エンジン側が「pending 化して再作成」にフォールバックできるようにする(仕様8章4)。
func (c *Client) UpdateBlocker(ctx context.Context, cal model.CalendarRef, eventID string, b model.Blocker) error {
	svc, err := c.service(ctx)
	if err != nil {
		return err
	}
	patch := &calendar.Event{
		Summary:     b.Title,
		Start:       blockerStart(b),
		End:         blockerEnd(b),
		Description: b.Description,
		// 空文字でのクリア(show_origin_in_description のトグル OFF)も
		// 反映するため、Description は常に明示送信する(Issue #7)
		ForceSendFields: []string{"Description"},
	}
```

(2) `CreateBlocker` の 409 非 cancelled 分岐を変更:

```go
			if existing.Status != "cancelled" {
				return existing.Id, nil
			}
```
↓
```go
			if existing.Status != "cancelled" {
				// クラッシュ再実行の 409 収容では、停止中に origin の内容が変わって
				// いる可能性がある(mappings には新しいハッシュが保存されるため、
				// ここで揃えないと食い違いが次の変更まで固定化する)。作成しようと
				// していた内容で patch してから返す(スペック 2026-07-15 §5)
				if err := c.UpdateBlocker(ctx, cal, existing.Id, b); err != nil {
					return "", fmt.Errorf("google[%s]: align existing blocker %s: %w", c.accountID, idemKey, err)
				}
				return existing.Id, nil
			}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/provider/google/ -race -count=1`
Expected: PASS(`TestBlockerDescriptionSentAndCleared` は PATCH ハンドラ登録済みなのでそのまま通る)

- [ ] **Step 5: コミット**

```bash
git add internal/provider/google/blockers.go internal/provider/google/blockers_test.go
git commit -m "feat: google patch sends summary and 409 adoption aligns blocker content"
```

---

### Task 6: Microsoft — 409 復旧で内容整合 PATCH

**Files:**
- Modify: `internal/provider/microsoft/blockers.go`(`CreateBlocker` の 409 分岐)
- Test: `internal/provider/microsoft/blockers_test.go`(既存 1 件更新)

**Interfaces:**
- Consumes: 同ファイルの `findBlockerByOriginTag` / `UpdateBlocker`(既存)
- Produces: 「CreateBlocker の 409 収容は内容を揃えてから ID を返す」契約(Google と対称)

- [ ] **Step 1: 既存テストを新契約に更新(失敗する状態を作る)**

`TestCreateBlockerConflictReturnsExistingID`(blockers_test.go:109)の mux に PATCH ハンドラを追加し、アサーションを 3 リクエストに変更:

```go
	var reqs []recordedRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/me/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":{"code":"ErrorDuplicateTransactionId","message":"duplicate transactionId"}}`)
			return
		}
		fmt.Fprint(w, `{"value":[{"id":"existing-ev-9"}]}`)
	})
	mux.HandleFunc("/me/events/existing-ev-9", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPatch, r.Method)
		fmt.Fprint(w, `{"id":"existing-ev-9"}`)
	})
	srv := httptest.NewServer(record(&reqs, mux.ServeHTTP))
	defer srv.Close()
```

アサーション部:

```go
	require.Len(t, reqs, 2)
	require.Equal(t, http.MethodGet, reqs[1].Method)
```
↓
```go
	require.Len(t, reqs, 3)
	require.Equal(t, http.MethodGet, reqs[1].Method)
	// クラッシュ再実行の収容では既存ブロッカーの内容が古い可能性があるため、
	// 作成しようとしていた内容で PATCH してから ID を返す(スペック 2026-07-15 §5)
	require.Equal(t, http.MethodPatch, reqs[2].Method)
	var patched map[string]any
	require.NoError(t, json.Unmarshal(reqs[2].Body, &patched))
	require.Equal(t, "予定あり", patched["subject"], "PATCH ボディに転記内容(subject)が入る")
	body, ok := patched["body"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "", body["content"], "description も揃える(この Blocker は説明なし)")
```

(`recordedRequest.Body []byte` は `delta_test.go:24-42` の既存 `record` ヘルパが記録する。$filter の検証など既存アサーションは reqs[1] のまま維持する)

さらに `TestFindBlockerByOriginTagEncodesSpacesAsPercent20`(blockers_test.go:151)は `CreateBlocker` の戻りに `require.NoError` があるため、PATCH 先が未登録だと ServeMux の 404 で失敗するようになる。mux に同形のハンドラを追加する:

```go
	mux.HandleFunc("/me/events/existing-ev-9", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"existing-ev-9"}`)
	})
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/provider/microsoft/ -race -count=1 -run ConflictReturnsExisting`
Expected: FAIL(リクエストが 2 件のまま)

- [ ] **Step 3: 実装**

`CreateBlocker` の 409 分岐:

```go
	case status == http.StatusConflict:
		// transactionId の再送(クラッシュ後の再実行)。既存ブロッカーをタグで特定する。
		return c.findBlockerByOriginTag(ctx, b.OriginTag)
```
↓
```go
	case status == http.StatusConflict:
		// transactionId の再送(クラッシュ後の再実行)。既存ブロッカーをタグで特定し、
		// 停止中に origin の内容が変わっている可能性に備えて、作成しようとしていた
		// 内容で PATCH してから返す(スペック 2026-07-15 §5。Google の 409 収容と対称)
		id, err := c.findBlockerByOriginTag(ctx, b.OriginTag)
		if err != nil {
			return "", err
		}
		if err := c.UpdateBlocker(ctx, cal, id, b); err != nil {
			return "", fmt.Errorf("graph create blocker: align existing %s: %w", id, err)
		}
		return id, nil
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/provider/microsoft/ -race -count=1`
Expected: PASS(Step 1 で `TestFindBlockerByOriginTagEncodesSpacesAsPercent20` にも PATCH ハンドラを追加済みであること)

- [ ] **Step 5: コミット**

```bash
git add internal/provider/microsoft/blockers.go internal/provider/microsoft/blockers_test.go
git commit -m "feat: graph 409 recovery aligns existing blocker content before adopting"
```

---

### Task 7: ドキュメント(v1 仕様書ポインタ・README・CHANGELOG)

**Files:**
- Modify: `docs/superpowers/specs/2026-07-03-calsync-design.md`(§1 と §3 に参照追記)
- Modify: `README.md`(13 行目・223 行目周辺・350 行目)
- Modify: `CHANGELOG.md`(`[Unreleased]` → `### Added` の先頭)

**Interfaces:** なし(ドキュメントのみ)

- [ ] **Step 1: v1 仕様書に例外参照を追記**

(1) §1(10 行目):

```markdown
- 相互ミラーリング型の Busy Blocker。予定の中身は同期しない(タイトル固定・詳細なし)
```
↓
```markdown
- 相互ミラーリング型の Busy Blocker。予定の中身は同期しない(タイトル固定・詳細なし)。例外はペア別オプトインの detail_sync([2026-07-15-detail-sync-design.md](2026-07-15-detail-sync-design.md))のみ
```

(2) §3 の表(50 行目)の「ブロッカー」行:

```markdown
| ブロッカー | 元予定 1 : ブロッカー 1(マージしない)、固定タイトル(既定「予定あり」)、リマインダーなし、visibility=private | 冪等性を優先 |
```
↓
```markdown
| ブロッカー | 元予定 1 : ブロッカー 1(マージしない)、固定タイトル(既定「予定あり」)、リマインダーなし、visibility=private。detail_sync ペアのみタイトル/説明を転記([2026-07-15-detail-sync-design.md](2026-07-15-detail-sync-design.md)) | 冪等性を優先 |
```

- [ ] **Step 2: README を改訂**

(1) 13 行目の特徴節:

```markdown
- 予定の中身は同期しません。タイトル固定(既定「予定あり」)・詳細なしのブロッカーだけを作ります
```
↓
```markdown
- 既定では予定の中身は同期しません。タイトル固定(既定「予定あり」)・詳細なしのブロッカーだけを作ります(`detail_sync` で明示したペアに限りタイトル/説明を転記できます)
```

(2) 223 行目の `show_origin_in_description` 節の直後に新しい小節を追加(見出しレベルは前後の節に合わせる):

```markdown
### ペア別にタイトル/説明も同期する(detail_sync)

既定ではブロッカーの中身は転記されませんが、「アカウント B の予定をアカウント A へミラーするときだけ、タイトル(と説明)も見たい」場合は、トップレベルの `detail_sync` でペアを明示します:

​```yaml
detail_sync:
  - from: work        # 元アカウント(accounts の id)
    to: personal      # ミラー先アカウント
    fields: [title, description]   # title / description から選択
  - from: sub
    to: personal
    fields: [title]
​```

- 方向は一方通行です(逆方向も欲しければ 2 エントリ書きます)。指定していないペアは従来どおり完全匿名のままです
- 元イベントのタイトル/説明の変更は次のポーリング(既定 1 分)で自動追従します
- ペアの追加・削除・fields 変更を既存ブロッカーに反映するには、デーモン再起動後の日次リコンサイルを待つか、デーモン停止中に `calsync reconcile` を実行します
- ブロッカーの visibility は private のままです。カレンダーを共有された第三者には従来どおり詳細は見えません(詳細が見えるのは自分のカレンダー上だけです)
```

(注: 上記コードフェンス内の `​` は計画書のネスト回避用。実際の README には通常の ``` を書く)

(3) 350 行目のプライバシー節:

```markdown
タイトル・本文・参加者など予定の中身は一切コピーされません。
```
↓
```markdown
タイトル・本文・参加者など予定の中身は、既定では一切コピーされません(`detail_sync` で明示的にオプトインしたペアに限り、タイトル/説明が転記されます)。
```

- [ ] **Step 3: CHANGELOG に追記**

`## [Unreleased]` → `### Added` の先頭に追加:

```markdown
- **ペア別タイトル/説明同期(`detail_sync`)**: トップレベル `detail_sync` で指定した origin => target アカウントペアに限り、ブロッカーのタイトル/説明を元イベントから転記(`fields: [title, description]` で選択)。既定は従来どおり完全匿名で、未設定なら保存ハッシュも従来と完全同一(アップグレード無風)。内容をハッシュに合成しているため元イベントの変更は次のポーリングで追従し、設定変更は次回リコンサイルで既存分にも遡及。併せて (1) Google の patch にタイトルを追加、(2) 両プロバイダの 409 復旧時に内容整合 patch を追加、(3) リコンサイル収容・再構築行は sentinel により 1 回だけ自己修復 patch されるように(ペア解除後に DB 再構築を挟んでも転記内容が残留しない)
```

- [ ] **Step 4: 検証とコミット**

Run: `docker compose config -q && go build -o ./calsync ./cmd/calsync`
Expected: エラーなし(ドキュメントのみの変更だが習慣として)

```bash
git add docs/superpowers/specs/2026-07-03-calsync-design.md README.md CHANGELOG.md
git commit -m "docs: document detail_sync in v1 spec, README privacy notes, and changelog"
```

---

### Task 8: 総合検証と PR

**Files:** なし(検証のみ)

- [ ] **Step 1: 全チェックを実行**

```bash
go build -o ./calsync ./cmd/calsync
go test ./... -race -count=1
go vet ./... && gofmt -l internal/ cmd/
docker compose config -q
```

Expected: ビルド成功・全テスト PASS・vet/gofmt 出力なし・compose OK

- [ ] **Step 2: スペック照合(セルフレビュー)**

スペック §2〜§8 の各主張に対応する実装/テストがあることを確認する。特に:
- ペア未設定の保存ハッシュが従来値(`TestUpsertBlockers_DetailSyncPerPair` の b 側アサーション)
- 冪等キー不変(`TestFullResync_DetailSyncTogglesRetroactively`)
- sentinel の無条件付与(`TestReconcile_RebuildSelfHealsBlockerContent` — ペアなしケース)

- [ ] **Step 3: push して PR 作成**

```bash
git push -u origin feat/detail-sync
gh pr create --title "feat: per-pair title/description sync (detail_sync)" --body "..."
```

PR 本文には: スペックへのリンク、ユーザー影響(既定無風・オプトイン)、実行したチェック、ライブ検証は別途(スペック §10 の手順: デーモン停止 → detail_sync 追記 → `calsync reconcile` → 起動 → 反映確認 → タイトル変更追従 → 解除復帰)と明記する。

---

## ライブ検証(PR マージ前・実運用環境で実施)

スペック §10 の手順。**書き込み系コマンドはデーモン停止中のみ**の運用制約に従う:

1. `launchctl bootout gui/$(id -u)/com.btajp.calsync`(デーモン停止)
2. `data/calsync.yaml` に `detail_sync` を 1 ペア追記(feature ブランチのバイナリを `~/.local/bin/calsync` に再ビルドしてから)
3. 停止中に `calsync reconcile --config ... --data ...` → 対象ペアの既存ブロッカーにタイトルが転記されることをカレンダーで確認
4. デーモン起動(`install-launchd.sh` 相当)→ origin でタイトル変更 → 約 1 分で追従を確認
5. 同手順で `detail_sync` を外して reconcile → 「予定あり」へ復帰を確認
6. 検証結果をスペックへ実測記録として追記
