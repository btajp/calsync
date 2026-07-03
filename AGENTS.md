# AGENTS.md — AI エージェント向けガイド

このリポジトリで作業する AI エージェント(Claude Code / Codex 等)向けの案内。人間向けの利用手順は [README.md](README.md) を参照。

## プロジェクト概要

calsync は複数の Google / Microsoft カレンダーを相互監視し、Busy 予定を他の全アカウントへ「予定あり」ブロッカーとしてミラーする Go 製セルフホストデーモン。設計の正は `docs/superpowers/specs/2026-07-03-calsync-design.md`(仕様書)、実装の由来は `docs/superpowers/plans/2026-07-03-calsync-v1.md`(実装計画)。**コードと仕様が食い違って見えたら仕様書を先に読むこと。**

## ビルド・テスト・検証

```bash
go build -o ./calsync ./cmd/calsync   # ビルド(CGO 不要)
go test ./... -race -count=1         # 全テスト(必ず -race で)
go vet ./... && gofmt -l internal/ cmd/   # 静的チェック(gofmt は出力なしが正)
docker compose config -q             # compose 構文チェック
```

- コミット前に必ず上記をすべて通すこと
- `go mod tidy` は**未使用の予約依存を消すため原則禁止**。依存の昇格は対象限定の `go get <module>@<既存バージョン>` で行う

## リポジトリ構成(要点)

| パス | 責務 |
| --- | --- |
| `internal/model` | 正規化イベント・TimeHash・冪等キー導出(全レイヤーの共有語彙) |
| `internal/config` | YAML ロード・検証(microsoft は primary のみ等の v1 制約もここ) |
| `internal/store` | SQLite(WAL・flock 多重起動防止・status/doctor 用 OpenReadOnly) |
| `internal/provider` | Provider IF・sentinel errors・認証エラー正規化(autherr.go) |
| `internal/provider/{google,microsoft}` | 実 API 実装。方言はここに閉じ込める(エンジンに漏らさない) |
| `internal/provider/fake` | エンジンテスト用インメモリ実装(**実プロバイダと同じ契約を守ること**) |
| `internal/engine` | 同期エンジン・リコンサイル・スケジューラ(プロバイダ非依存) |
| `cmd/calsync` | cobra CLI |

## 壊してはいけない不変条件

1. **ループ防止**: mappings テーブルが一次判定、拡張プロパティタグは二次(リカバリ用)。削除通知は id しか持たないためタグに依存した削除判定は不可
2. **カーソル規律**: 差分取得を完走したときだけカーソルを永続化。カーソル失効(Google 410 / Graph 410・syncStateNotFound)時も **mappings は絶対にワイプしない**(カーソルとイベントキャッシュのみ破棄 → set-difference で自己修復)
3. **冪等作成**: ブロッカー作成は必ず冪等キー(Google: クライアント生成 ID / Graph: transactionId)+ mappings の pending→active 遷移を経る
4. **Graph の作法**: 全リクエストに `Prefer: IdType="ImmutableId"`、`odata.maxpagesize` 禁止、OData クエリの空白は `%20`(`+` は拒否される)
5. **OAuth リダイレクト**: Microsoft は「localhost・パスなし」形式必須(MSA はポートを無視するがパスを照合)。認可 URL には `prompt=select_account` を必ず付ける(同意済みセッションの無言再発行で別アカウントのトークンが保存された実績あり)

## ドキュメント規約

- **図はすべて Mermaid**。ASCII アート図は禁止
- コミットは Conventional Commits(feat/fix/docs/test/build/chore、英語)
- 変更したら `CHANGELOG.md`(Keep a Changelog 形式)の `[Unreleased]` に追記
- 実 API の未検証事項は仕様書 15 章のスパイクチェックリストで管理。実測で確認したら消し込む

## スキル

エージェント向けスキルの実体は `.agents/skills/` にある(`.claude/skills` はそこへのシンボリックリンク)。ユーザーのセットアップ・アカウント追加を支援する際は `calsync-setup` スキルを必ず参照すること(組織/個人アカウントの分岐・ブラウザプロファイルの注意・実測済みの落とし穴を集約してある)。

## 既知の落とし穴(実測済み)

- GCP OAuth 同意画面が Testing のままだと refresh token が7日失効 → External は必ず「In production」に publish
- Google Workspace 管理対象アカウントは未検証アプリ+機微スコープを強制ブロック(個人アカウントと違いクリックスルー不可)→ 管理コンソールの「API の制御 → アプリのアクセス制御」で client ID を「信頼できる」に登録
- Entra アプリ登録直後は MSA(login.live.com)への伝播ラグで `invalid_request` が出ることがある → 数分待って再試行
- SQLite は flock で単一プロセス前提。デーモン稼働中に書き込み系コマンド(sync/reconcile/accounts remove)を並行実行しない(status/doctor は読み取り専用で安全)
