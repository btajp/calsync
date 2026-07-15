# detail_sync visibility(ペア別公開設定)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** detail_sync ペアに任意キー `visibility`(private | default | public)を追加し、ブロッカーの公開設定をペア別に制御できるようにする。

**Architecture:** `DetailSyncPair.Visibility`(正規化済み)→ `model.Blocker.Visibility`(空文字 = private)→ プロバイダ写像(Google: そのまま / Graph: private 以外 normal)。変更検知は `detailComponentFor` の返り値末尾に `+vis:<値>` を private 以外のときだけ追加(既存ペアは無風)。

**Tech Stack:** Go(新規依存なし)、DB スキーマ変更なし。

**設計の正:** `docs/superpowers/specs/2026-07-15-detail-sync-design.md` **§12**

## Global Constraints

- ビルド: `go build -o ./calsync ./cmd/calsync` / テスト: `go test ./... -race -count=1` / 静的: `go vet ./... && gofmt -l internal/ cmd/`(出力なし)/ `docker compose config -q`
- `go mod tidy` 禁止。コミットは Conventional Commits(英語)。作業ブランチ `feat/detail-sync-visibility`
- 冪等キー・`model.TimeHash`・`model.DetailHash` は一切変更しない
- **無風保証**: visibility 未指定(= private)のペアは、保存ハッシュ・送信ボディとも現状と完全同一(稼働中の既存 detail_sync ペアに一斉 patch を起こさない)
- `Blocker.Visibility` の空文字は private 扱い(全プロバイダ)
- vis 成分の合成は `detailComponentFor` 1 箇所に閉じる(合成順: `+detail:<hex>` の直後)

---

### Task 1: config — visibility キーと検証

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `DetailSyncPair.Visibility string`(正規化済み: "private" | "default" | "public"。未指定は "private")— Task 2 のエンジンが使う

- [ ] **Step 1: 失敗するテストを書く**

`internal/config/config_test.go` の `TestLoad` テーブルに追加(detail_sync の既存ケース群の直後):

```go
		{
			name: "detail_sync visibility parsed and normalized",
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
    visibility: default
  - from: b
    to: a
    fields: [title]
`,
			check: func(t *testing.T, c *Config) {
				require.Equal(t, "default", c.DetailSync[0].Visibility)
				require.Equal(t, "private", c.DetailSync[1].Visibility, "未指定は private に正規化")
			},
		},
		{
			name: "detail_sync invalid visibility is rejected",
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
    visibility: secret
`,
			wantErr: `invalid visibility "secret" (want private, default, or public)`,
		},
		{
			name: "detail_sync unknown visibility-like key is rejected by KnownFields",
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
    visibilty: default
`,
			wantErr: "field visibilty not found",
		},
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/config/ -race -count=1`
Expected: FAIL(`Visibility` 未定義のコンパイルエラー)

- [ ] **Step 3: 実装**

(1) `DetailSyncPair`(config.go:67-70)にフィールドとコメントを追加:

```go
// DetailSyncPair は detail_sync の 1 エントリ(スペック 2026-07-15 §2, §12)。
// origin(From)→ target(To)アカウントの一方通行のペアで、指定したペアに限り
// ブロッカーのタイトル/説明を元イベントから転記する(既定は完全匿名のまま)。
// fields は検証時に bool へ正規化する(正規順 title → description はハッシュ側で担保)。
// Visibility は検証時に正規化済み("private" | "default" | "public"。未指定は "private")。
type DetailSyncPair struct {
	From, To           string
	Title, Description bool
	Visibility         string
}
```

(2) `rawDetailSync`(config.go:121-125)に追加:

```go
type rawDetailSync struct {
	From       string   `yaml:"from"`
	To         string   `yaml:"to"`
	Fields     []string `yaml:"fields"`
	Visibility string   `yaml:"visibility"`
}
```

(3) 検証ループ内、fields の for 文の直後(`cfg.DetailSync = append(cfg.DetailSync, p)` の直前)に追加:

```go
		switch rd.Visibility {
		case "":
			p.Visibility = "private" // 未指定 = 従来どおり非公開(スペック §12.1)
		case "private", "default", "public":
			p.Visibility = rd.Visibility
		default:
			return nil, fmt.Errorf("config: detail_sync[%d]: invalid visibility %q (want private, default, or public)", i, rd.Visibility)
		}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/config/ -race -count=1`
Expected: PASS(既存ケース含め全部)

- [ ] **Step 5: コミット**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add per-pair visibility setting to detail_sync config"
```

---

### Task 2: model + engine — Blocker.Visibility と vis 成分の合成

**Files:**
- Modify: `internal/model/model.go`(Blocker struct)
- Modify: `internal/engine/engine.go`(`detailComponentFor` / `blockerFor`)
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `DetailSyncPair.Visibility`(Task 1)
- Produces: `model.Blocker.Visibility string`(空文字 = private)— Task 3 のプロバイダが写像する

- [ ] **Step 1: 失敗するテストを書く**

`internal/engine/engine_test.go` の detail_sync テスト群の末尾に追加:

```go
// enableDetailSyncVisibility は visibility 付きのペアを設定する(config の正規化済み値を模す)。
func enableDetailSyncVisibility(e *Engine, from, to string, title, description bool, visibility string) {
	e.Cfg.DetailSync = append(e.Cfg.DetailSync,
		config.DetailSyncPair{From: from, To: to, Title: title, Description: description, Visibility: visibility})
}

// visibility=default のペアはブロッカーに Visibility が付き、ハッシュに +vis: 成分が入る。
// visibility=private(未指定と同義)のペアは §3 の従来ハッシュと完全一致(無風保証 — スペック §12.3)
func TestUpsertBlockers_DetailSyncVisibility(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSyncVisibility(e, "a", "c", true, false, "default")

	ev := detailEvent("ev1")
	require.NoError(t, e.processEvent(ctx, refA, ev))

	// c: Visibility が Blocker に載る
	bc, ok := f.StoredBlocker(calCv, f.Blockers(calCv)[0].EventID)
	require.True(t, ok)
	require.Equal(t, "default", bc.Visibility)

	// b(ペアなし): Visibility は空(= private 扱い)
	bb, ok := f.StoredBlocker(calBv, f.Blockers(calBv)[0].EventID)
	require.True(t, ok)
	require.Empty(t, bb.Visibility)

	// ハッシュ: c は +vis:default 付き、b は素の TimeHash
	mc, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)
	require.Equal(t,
		model.TimeHash(ev)+"+detail:"+model.DetailHash(true, false, ev.Title, ev.Description)+"+vis:default",
		mc.TimeHash)
	mb, err := e.Store.GetMapping("a", "primary", "ev1", "b")
	require.NoError(t, err)
	require.Equal(t, model.TimeHash(ev), mb.TimeHash)
}

// visibility=private のペアは vis 成分なし = visibility 導入前のハッシュと完全一致(無風保証)。
// テストがペアを直接構築して Visibility="" になる場合も同じ扱いであること
func TestDetailComponent_PrivateVisibilityIsNoop(t *testing.T) {
	e, _ := newTestEngine(t)
	ev := detailEvent("ev1")
	want := "+detail:" + model.DetailHash(true, true, ev.Title, ev.Description)

	enableDetailSyncVisibility(e, "a", "c", true, true, "private")
	require.Equal(t, want, e.detailComponentFor("a", "c", ev), `"private" は vis 成分なし`)

	e.Cfg.DetailSync = nil
	enableDetailSync(e, "a", "c", true, true) // Visibility ゼロ値("")のペア
	require.Equal(t, want, e.detailComponentFor("a", "c", ev), `"" も private と同義`)
}

// visibility トグルの遡及(FullResync 経由)と冪等キー・件数不変
func TestFullResync_VisibilityToggleRetroactively(t *testing.T) {
	e, f := newTestEngine(t)
	ctx := context.Background()
	enableDetailSync(e, "a", "c", true, false) // visibility 未指定(= private)で作成

	ev := detailEvent("ev1")
	f.SetFullState(refA, []model.NormalizedEvent{ev})
	require.NoError(t, e.SyncCalendar(ctx, refA))
	blks := f.Blockers(calCv)
	require.Len(t, blks, 1)
	m0, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)

	// private → public
	e.Cfg.DetailSync[0].Visibility = "public"
	require.NoError(t, e.FullResync(ctx, refA))
	b1, ok := f.StoredBlocker(calCv, blks[0].EventID)
	require.True(t, ok)
	require.Equal(t, "public", b1.Visibility)

	// public → private(既定へ復帰)
	e.Cfg.DetailSync[0].Visibility = "private"
	require.NoError(t, e.FullResync(ctx, refA))
	b2, ok := f.StoredBlocker(calCv, blks[0].EventID)
	require.True(t, ok)
	require.Equal(t, "private", b2.Visibility, "blockerFor はペアの正規化済み値をそのまま載せる")

	// 冪等キー・件数はトグル前後で不変
	m2, err := e.Store.GetMapping("a", "primary", "ev1", "c")
	require.NoError(t, err)
	require.Equal(t, m0.IdempotencyKey, m2.IdempotencyKey)
	require.Len(t, f.Blockers(calCv), 1)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/engine/ -race -count=1 -run 'Visibility'`
Expected: FAIL(`Visibility` 未定義のコンパイルエラー)

- [ ] **Step 3: 実装**

(1) `internal/model/model.go` の `Blocker` struct、`Description` フィールドの直後に追加:

```go
	Visibility     string // "private"(空文字も同義)| "default" | "public"。detail_sync ペアの設定からのみ非 private になる(スペック 2026-07-15 §12)
```

(2) `internal/engine/engine.go` の `detailComponentFor` の return を変更:

```go
	return "+detail:" + model.DetailHash(p.Title, p.Description, ev.Title, ev.Description)
```
↓
```go
	comp := "+detail:" + model.DetailHash(p.Title, p.Description, ev.Title, ev.Description)
	// visibility 成分は private(既定・空文字含む)のとき付けない — 既存ペアの
	// 保存ハッシュを変えないため(スペック §12.3 の無風保証)
	if p.Visibility != "" && p.Visibility != "private" {
		comp += "+vis:" + p.Visibility
	}
	return comp
```

(3) `blockerFor` を変更: `visibility` 変数を導入してペアから解決し、Blocker に載せる。

```go
	title := e.Cfg.BlockerTitle
	desc := e.descriptionFor(targetCal.AccountID, originTag)
	originID, _, _ := strings.Cut(originTag, ":")
	if pol := e.Cfg.DetailSyncFor(originID, targetCal.AccountID); pol != nil {
```
↓
```go
	title := e.Cfg.BlockerTitle
	desc := e.descriptionFor(targetCal.AccountID, originTag)
	visibility := "" // ペアなしは空文字 = private(プロバイダ側で写像)
	originID, _, _ := strings.Cut(originTag, ":")
	if pol := e.Cfg.DetailSyncFor(originID, targetCal.AccountID); pol != nil {
		visibility = pol.Visibility
```

および返り値の struct リテラルに追加(`Description: desc,` の直後):

```go
		Visibility:     visibility,
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/engine/ -race -count=1 && go test ./internal/model/ -race -count=1`
Expected: PASS(既存の detail_sync テスト・reconcile テスト含め全部 — 既存ペアはすべて Visibility ゼロ値なのでハッシュ不変)

- [ ] **Step 5: コミット**

```bash
git add internal/model/model.go internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat: carry per-pair visibility into blocker payload and change hash"
```

---

### Task 3: providers — Google visibility / Graph sensitivity 写像

**Files:**
- Modify: `internal/provider/google/blockers.go`(`blockerEventBody` / `UpdateBlocker`)
- Modify: `internal/provider/microsoft/blockers.go`(`blockerBody`)
- Test: `internal/provider/google/blockers_test.go`, `internal/provider/microsoft/blockers_test.go`

**Interfaces:**
- Consumes: `model.Blocker.Visibility`(Task 2)
- Produces: なし(送信ボディの変更のみ。fake は body 全置換のため変更不要)

- [ ] **Step 1: 失敗するテストを書く**

(1) `internal/provider/google/blockers_test.go` に追加:

```go
// Blocker.Visibility の写像: 空文字/private → private、default/public はそのまま(スペック §12.2)。
// insert と patch の両方で送られる(patch はトグルの遡及反映用)
func TestBlockerVisibilityMapping(t *testing.T) {
	cases := []struct {
		visibility string
		want       string
	}{
		{"", "private"},
		{"private", "private"},
		{"default", "default"},
		{"public", "public"},
	}
	for _, tc := range cases {
		t.Run("visibility="+tc.want, func(t *testing.T) {
			var created, patched map[string]any
			mux := http.NewServeMux()
			mux.HandleFunc("POST /calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&created)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"blk1","status":"confirmed"}`)
			})
			mux.HandleFunc("PATCH /calendars/primary/events/blk1", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&patched)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"blk1"}`)
			})
			c := newTestClient(t, mux)

			b := model.Blocker{
				Title:      "予定あり",
				StartUTC:   time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
				EndUTC:     time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
				OriginTag:  "a:ev1",
				Visibility: tc.visibility,
			}
			_, err := c.CreateBlocker(context.Background(), testRef, b, "csidem1")
			require.NoError(t, err)
			require.Equal(t, tc.want, created["visibility"], "insert ボディの visibility")

			require.NoError(t, c.UpdateBlocker(context.Background(), testRef, "blk1", b))
			require.Equal(t, tc.want, patched["visibility"], "patch ボディの visibility(遡及反映用に常時送信)")
		})
	}
}
```

(2) 既存 `TestUpdateBlockerPatchesTimesAndDescription` のキー集合アサーションを更新:

```go
	require.ElementsMatch(t, []string{"start", "end", "description", "summary"}, mapKeys(m),
		"patch は start/end/description/summary(detail_sync のタイトル追従)")
```
↓
```go
	require.ElementsMatch(t, []string{"start", "end", "description", "summary", "visibility"}, mapKeys(m),
		"patch は start/end/description/summary/visibility(detail_sync の追従)")
```

(3) `internal/provider/microsoft/blockers_test.go` に追加:

```go
// Blocker.Visibility → Graph sensitivity の写像: 空文字/private → private、
// default/public → normal(Graph に「公開」の段階は無い。スペック §12.2)
func TestBlockerSensitivityMapping(t *testing.T) {
	cases := []struct {
		visibility string
		want       string
	}{
		{"", "private"},
		{"private", "private"},
		{"default", "normal"},
		{"public", "normal"},
	}
	for _, tc := range cases {
		t.Run("visibility="+tc.visibility, func(t *testing.T) {
			var reqs []recordedRequest
			handler := func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"id":"ev-new"}`)
			}
			srv := httptest.NewServer(record(&reqs, handler))
			defer srv.Close()

			c := newTestClient(t, srv.URL, []string{"busy"})
			b := model.Blocker{
				Title:      "予定あり",
				StartUTC:   time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC),
				EndUTC:     time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC),
				OriginTag:  "a:ev1",
				Visibility: tc.visibility,
			}
			_, err := c.CreateBlocker(context.Background(), testCal, b, "calsync-vis1")
			require.NoError(t, err)

			require.Len(t, reqs, 1)
			var body map[string]any
			require.NoError(t, json.Unmarshal(reqs[0].Body, &body))
			require.Equal(t, tc.want, body["sensitivity"])
		})
	}
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/provider/google/ ./internal/provider/microsoft/ -race -count=1 -run 'Mapping|UpdateBlockerPatches'`
Expected: FAIL(visibility が常に "private" / patch に visibility キーなし)

- [ ] **Step 3: 実装**

(1) `internal/provider/google/blockers.go` — `blockerEventBody` の直前にヘルパを追加し、ハードコードを置換:

```go
// googleVisibility は Blocker.Visibility を Google の visibility 値へ写像する。
// 空文字(ペアなし・既定)は private = 従来の匿名ブロッカー(スペック 2026-07-15 §12.2)。
// それ以外は config 検証済みの値(private/default/public)をそのまま使う。
func googleVisibility(b model.Blocker) string {
	if b.Visibility == "" {
		return "private"
	}
	return b.Visibility
}
```

`blockerEventBody` 内:
```go
		Visibility:   "private",
```
↓
```go
		Visibility:   googleVisibility(b),
```

`UpdateBlocker` の patch struct に追加(`Summary:` の直後。写像後の値は常に非空なので ForceSendFields 不要):
```go
		Visibility:  googleVisibility(b),
```

(2) `internal/provider/microsoft/blockers.go` — `blockerBody` の直前にヘルパを追加し、ハードコードを置換:

```go
// graphSensitivity は Blocker.Visibility を Graph の sensitivity へ写像する。
// Graph に「公開」の段階は無いため、private 以外(default/public)はすべて
// normal = 普通の予定(スペック 2026-07-15 §12.2)。空文字は private(既定)。
func graphSensitivity(b model.Blocker) string {
	if b.Visibility == "" || b.Visibility == "private" {
		return "private"
	}
	return "normal"
}
```

`blockerBody` 内:
```go
		Sensitivity:   "private",
```
↓
```go
		Sensitivity:   graphSensitivity(b),
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/provider/google/ ./internal/provider/microsoft/ -race -count=1`
Expected: PASS(既存テストは Visibility ゼロ値 → "private" のため不変)

- [ ] **Step 5: コミット**

```bash
git add internal/provider/google/blockers.go internal/provider/google/blockers_test.go internal/provider/microsoft/blockers.go internal/provider/microsoft/blockers_test.go
git commit -m "feat: map per-pair visibility to google visibility and graph sensitivity"
```

---

### Task 4: ドキュメントと総合検証

**Files:**
- Modify: `README.md`(detail_sync 節)
- Modify: `CHANGELOG.md`

- [ ] **Step 1: README の detail_sync 節を更新**

(1) YAML 例の 1 エントリ目に visibility を追加:

```yaml
detail_sync:
  - from: work-ms     # 元アカウント(accounts の id)
    to: personal      # ミラー先アカウント
    fields: [title, description]   # title / description から選択
  - from: personal
    to: work-ms
    fields: [title]
```
↓
```yaml
detail_sync:
  - from: work-ms     # 元アカウント(accounts の id)
    to: personal      # ミラー先アカウント
    fields: [title, description]   # title / description から選択
    visibility: default             # private(既定)| default | public
  - from: personal
    to: work-ms
    fields: [title]
```

(2) 同節の箇条書きの「ブロッカーの visibility は private のままです。…」の直前に追加:

```markdown
- `visibility` でペアのブロッカーの公開設定を変えられます: `private`(既定 — 非公開のまま)/ `default`(カレンダーの共有設定に従う = 普通の予定と同じ)/ `public`(共有相手が時間枠のみ表示でも詳細を見せる)。Microsoft がミラー先の場合、default/public はどちらも通常の予定(sensitivity: normal)になります。変更は次回リコンサイルで既存ブロッカーにも遡及します
```

(3) 直後の既存箇条書きの冒頭を条件付きに修正:

```markdown
- ブロッカーの visibility は private のままです。閲覧権限だけの共有相手には従来どおり非公開の予定としか見えませんが、
```
↓
```markdown
- `visibility` 未指定のペアと通常のブロッカーは private のままです。閲覧権限だけの共有相手には従来どおり非公開の予定としか見えませんが、
```

- [ ] **Step 2: CHANGELOG に追記**

`## [Unreleased]` → `### Added` の先頭に:

```markdown
- **detail_sync のペア別 visibility**: `detail_sync[].visibility`(`private`(既定)/ `default` / `public`)でペアのブロッカーの公開設定を制御可能に(Google: visibility / Microsoft: sensitivity へ写像、default・public はどちらも normal)。未指定は従来どおり非公開で、既存設定への影響なし(無風)。変更は次回リコンサイルで既存ブロッカーにも遡及
```

- [ ] **Step 3: 総合検証**

```bash
go build -o ./calsync ./cmd/calsync
go test ./... -race -count=1
go vet ./... && gofmt -l internal/ cmd/
docker compose config -q
```

Expected: すべて PASS / 出力なし

- [ ] **Step 4: コミット**

```bash
git add README.md CHANGELOG.md
git commit -m "docs: document per-pair visibility for detail_sync"
```
