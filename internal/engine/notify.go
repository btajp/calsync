package engine

import (
	"context"
	"errors"
	"log"
	"slices"
	"sort"
	"time"

	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/notify"
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
		return nextReconcileAt(now, hhmm)
	}
	entries, failed := e.collectDigest(ctx, day)
	if err := e.Notifier.SendDigest(ctx, day, entries, failed); err != nil {
		if errors.Is(err, notify.ErrNonRetryable) {
			log.Printf("digest: %v (non-retryable; skipping until tomorrow)", err)
			return nextReconcileAt(now, hhmm)
		}
		log.Printf("digest: %v (retrying next tick)", err)
		return scheduled
	}
	return nextReconcileAt(now, hhmm)
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
