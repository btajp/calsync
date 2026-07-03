# Changelog

このプロジェクトの特筆すべき変更はすべてこのファイルに記録します。

フォーマットは [Keep a Changelog](https://keepachangelog.com/ja/1.1.0/) に、バージョニングは [Semantic Versioning](https://semver.org/lang/ja/) に従います。

## [Unreleased]

### Added

- **相互 Busy ブロッカー同期エンジン**: 複数の Google カレンダー / Microsoft 365(個人 Microsoft アカウント含む)を差分ポーリング(Google: syncToken / Graph: calendarView delta)で監視し、Busy 予定を他の全アカウントへ「予定あり」ブロッカーとしてミラーする Hub & Spoke 構成
- **無限ループ防止**: ローカル mappings テーブルによる一次判定+イベント拡張プロパティタグ(`calsync-origin`)による二次判定・自己修復
- **冪等なブロッカー作成**: Google はクライアント生成イベント ID、Microsoft は `transactionId` による二重作成防止(クラッシュ・再送に安全)
- **同一会議の重複抑止**: 自分の複数アカウントが同じ会議に招待されている場合、iCalUID 照合でブロッカーを抑止(`dedupe_same_meeting`、既定オン)。実予定が消えた場合は自動昇格
- **リコンサイル**(日次+`calsync reconcile`): 同期ウィンドウのスライド、set-difference による差分照合、孤児ブロッカーの収容・掃除、手動削除されたブロッカーの復元、pending 状態の解決、DB 全損時のタグからの再構築
- **カーソル失効の自己回復**: Google 410 / Graph 410・`syncStateNotFound` を検出してフル再同期(mappings は破壊しない)
- **アカウント単位の障害分離**: トークン失効したアカウントだけ `reauth_required` で停止し他は継続。再認証後はリコンサイルで自動バックフィル
- **OAuth**: 認可コード+PKCE+ループバック(Google / Microsoft)、Device Code フロー(Microsoft、`--device-code`)、refresh token ローテーション追従の永続化
- **CLI**: `run` / `sync --once` / `reconcile` / `status` / `doctor` / `auth add`・`auth list` / `accounts remove --force`
- **状態管理**: SQLite 1 ファイル(WAL、flock による多重起動防止、`status`/`doctor` 用の読み取り専用オープン)
- **配布**: multi-stage Dockerfile(distroless、CGO 無効)、docker-compose 例、セットアップ要件を網羅した README(GCP「In production」必須手順、Entra アプリ登録、Docker での認証手順、プライバシー注記)

### 既知の制約(v1)

- Microsoft アカウントはプライマリカレンダーのみ監視・書き込み可
- 過去方向の同期はしない。ブロッカーのマージはしない(元予定 1 : ブロッカー 1)
- 単一インスタンス前提(同一データディレクトリでの多重起動は flock で拒否)
- 実 Google / Microsoft API に対する疎通確認は未実施(設計書 15 章のスパイクチェックリスト参照)
