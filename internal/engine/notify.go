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
	MeetingURL  string
	Description string // ダイジェストの blocks では使わない(リマインド用。v2 スペック 4 章)
	HTMLLink    string
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
// 収集する(スペック 5 章)。1 日窓で CollectWindow を呼ぶだけの薄い委譲(デスクトップ
// カレンダービュー設計 2026-07-21 §2)。
func (e *Engine) collectDigest(ctx context.Context, day time.Time) ([]DigestEntry, []string) {
	return e.CollectWindow(ctx, model.Window{Start: day, End: day.AddDate(0, 0, 1)})
}

// CollectWindow は任意の窓 w の実予定をライブ取得で収集する(旧 collectDigest の
// 一般化。デスクトップカレンダービュー設計 2026-07-21 §2)。newCursor は捨てる
// (カーソル規律に抵触しない — ダイジェストで実証済みのパターン)。
// 戻り値 failed は取得に失敗したアカウント ID(設定順・重複なし)。
func (e *Engine) CollectWindow(ctx context.Context, w model.Window) ([]DigestEntry, []string) {
	// 終日判定の窓を現地日付の閉区間 [winStartDate, winEndDateInclusive] で表す
	// (スペック: 「w.Start の現地日付」〜「w.End の前日の現地日付」)。w.End は
	// 半開区間の排他境界(=次に含まれない瞬間)なので、1 日引くとその窓が実際に
	// カバーする最終日になる。1 日窓(w.End = day+1日)に適用すると
	// winStartDate == winEndDateInclusive == dayStr となり、旧 digestIncludes の
	// dayStr 単一日比較と完全に一致する。
	winStartDate := w.Start.Format("2006-01-02")
	winEndDateInclusive := w.End.AddDate(0, 0, -1).Format("2006-01-02")

	var (
		entries []DigestEntry
		failed  []string
	)
	byKey := make(map[string]int)
	for _, acct := range e.Cfg.Accounts {
		acctFailed := false
		p, err := e.providerFor(acct.ID)
		if err != nil {
			log.Printf("collect window %s: %v", acct.ID, err)
			failed = append(failed, acct.ID)
			continue
		}
		// digest_calendars は監視対象(Calendars)には含まれない通知専用カレンダー。
		// ライブ取得にだけ acct.Calendars の後ろに連結して参加させる
		// (フィルタ・dedupe・failed 集約は既存のループをそのまま流用する。スペック 2 章)。
		digestCalIDs := make([]string, 0, len(acct.Calendars)+len(acct.DigestCalendars))
		digestCalIDs = append(digestCalIDs, acct.Calendars...)
		digestCalIDs = append(digestCalIDs, acct.DigestCalendars...)
		for _, calID := range digestCalIDs {
			if ctx.Err() != nil {
				return entries, failed
			}
			ref := model.CalendarRef{AccountID: acct.ID, CalendarID: calID}
			evs, _, err := p.Changes(ctx, ref, "", w)
			if err != nil {
				log.Printf("collect window %s: %v", ref, err)
				acctFailed = true
				break
			}
			for _, ev := range evs {
				include, err := e.windowIncludes(ref, ev, w, winStartDate, winEndDateInclusive)
				if err != nil {
					log.Printf("collect window %s: %v", ref, err)
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

// windowIncludes は 1 イベントが窓 w の対象かを判定する(旧 digestIncludes の一般化。
// デスクトップカレンダービュー設計 2026-07-21 §2)。
// 除外: 削除・辞退・ブロッカー(mappings 一次 + タグ二次。Graph delta はタグを
// 返せないため二次判定はライブ取得経路では Google のみ有効)。free は含める。
// 時刻指定: UTC 区間の重なり(w.Start/w.End そのもの)。
// 終日: 現地日付範囲の交差([winStartDate, winEndDateInclusive] と
// [AllDayStart, AllDayEnd) の交差。AllDayEnd 空は単日イベントとして扱う)。
// Window.Contains の終日 UTC 近似は日付境界で前日/翌日を誤包含しうるため使わない。
func (e *Engine) windowIncludes(ref model.CalendarRef, ev model.NormalizedEvent, w model.Window, winStartDate, winEndDateInclusive string) (bool, error) {
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
			return winStartDate <= ev.AllDayStart && ev.AllDayStart <= winEndDateInclusive, nil
		}
		return ev.AllDayStart <= winEndDateInclusive && winStartDate < ev.AllDayEnd, nil
	}
	return ev.EndUTC.After(w.Start) && ev.StartUTC.Before(w.End), nil
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
		MeetingURL:  ev.MeetingURL,
		Description: ev.Description,
		HTMLLink:    ev.HTMLLink,
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
		// Title と HTMLLink は同一アカウントからペアで採用する(v2 スペック 4 章):
		// 最初に HTMLLink が非空のアカウントの (Title, HTMLLink) を使う。
		// 全アカウントで HTMLLink が空の間は Title のみ v1 規則(最初の非空)で埋める
		if ex.HTMLLink == "" {
			if ev.HTMLLink != "" {
				ex.Title, ex.HTMLLink = ev.Title, ev.HTMLLink
			} else if ex.Title == "" && ev.Title != "" {
				ex.Title = ev.Title
			}
		}
		if ex.MeetingURL == "" {
			ex.MeetingURL = ev.MeetingURL
		}
		if ex.Description == "" {
			ex.Description = ev.Description
		}
		return
	}
	byKey[key] = len(*entries)
	*entries = append(*entries, entry)
}

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
			Title:       u.Title,
			StartUTC:    u.StartUTC,
			EndUTC:      u.EndUTC,
			MeetingURL:  u.MeetingURL,
			Description: u.Description,
			HTMLLink:    u.HTMLLink,
			AccountIDs:  []string{u.Ref.AccountID},
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
