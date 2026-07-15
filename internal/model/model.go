package model

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type Window struct {
	Start time.Time // 含む(end > Start のイベントが対象)
	End   time.Time // 含まない(start < End のイベントが対象)
}

type CalendarRef struct {
	AccountID  string
	CalendarID string // Google: "primary" 等 / Microsoft: 常に "primary"(v1)
}

type NormalizedEvent struct {
	ID          string // プロバイダのイベントID(opaque。パース禁止)
	ICalUID     string
	Title       string // 件名(Slack 通知の表示専用。TimeHash には含めない — スペック 4.1)
	MeetingURL  string // 会議 URL(v2 スペック 3.2 の規則で抽出。表示時に URL 検証を通す)
	Description string // 本文プレーンテキスト(Graph: Prefer で text 化 / Google: 簡易 HTML 除去済み)
	HTMLLink    string // カレンダー上の当該予定への URL(Google: htmlLink / Graph: webLink)
	StartUTC    time.Time
	EndUTC      time.Time
	IsAllDay    bool
	AllDayStart string // "2006-01-02"(IsAllDay時のみ。現地日付)
	AllDayEnd   string // 排他的終了日
	IsBusy      bool
	IsDeclined  bool
	Deleted     bool   // cancelled / @removed / isCancelled
	OriginTag   string // calsync タグが読めた場合のみ(Graph delta では常に "")
}

type Blocker struct {
	Title          string
	StartUTC       time.Time
	EndUTC         time.Time
	IsAllDay       bool
	AllDayStart    string
	AllDayEnd      string
	TargetTimezone string // 終日ブロッカー作成用(Graph はこのTZの midnight 境界で作る)
	OriginTag      string
	Description    string // 空なら説明なし(既定)。ターゲット側のオプトインで origin 情報を記載(Issue #7)
}

type BlockerRecord struct {
	EventID   string
	OriginTag string
	TimeHash  string
}

// OriginTag は "<origin_account_id>:<origin_event_id>"
func OriginTagOf(accountID, eventID string) string { return accountID + ":" + eventID }

// TimeHash: 予定時刻の変更検出用。16桁hex。
func TimeHash(ev NormalizedEvent) string {
	var s string
	if ev.IsAllDay {
		s = "allday|" + ev.AllDayStart + "|" + ev.AllDayEnd
	} else {
		s = ev.StartUTC.UTC().Format(time.RFC3339) + "|" + ev.EndUTC.UTC().Format(time.RFC3339)
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

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

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuv0123456789").WithPadding(base32.NoPadding) // 疑似base32hex(Google許容文字 a-v0-9)

// GoogleBlockerID: Google events.insert の id に指定するクライアント生成ID(冪等キー)。
func GoogleBlockerID(originTag, targetAccount string) string {
	sum := sha256.Sum256([]byte("gcal|" + originTag + "|" + targetAccount))
	return "cs" + b32.EncodeToString(sum[:20])
}

// MSTransactionID: Graph イベント作成の transactionId(冪等キー)。
func MSTransactionID(originTag, targetAccount string) string {
	sum := sha256.Sum256([]byte("msgraph|" + originTag + "|" + targetAccount))
	return "calsync-" + hex.EncodeToString(sum[:16])
}

func (w Window) Contains(ev NormalizedEvent) bool {
	if ev.IsAllDay {
		// 終日は現地日付だが、境界判定は日付をUTC日付として近似してよい(仕様5.3)
		start, err1 := time.Parse("2006-01-02", ev.AllDayStart)
		end, err2 := time.Parse("2006-01-02", ev.AllDayEnd)
		if err1 != nil || err2 != nil {
			return false
		}
		return end.After(w.Start) && start.Before(w.End)
	}
	return ev.EndUTC.After(w.Start) && ev.StartUTC.Before(w.End)
}

func (r CalendarRef) String() string { return fmt.Sprintf("%s/%s", r.AccountID, r.CalendarID) }
