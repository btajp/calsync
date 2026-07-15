package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btajp/calsync/internal/config"
	"github.com/btajp/calsync/internal/model"
	"github.com/btajp/calsync/internal/provider"
	"github.com/btajp/calsync/internal/store"
)

// FullResync は 1 カレンダーに対するカーソル張り直し + set-difference リコンサイル
// (仕様8章 1〜2)。cursor="" のウィンドウ付きフル同期で新カーソルを確立し
// (= ウィンドウのスライド)、フル同期結果と events キャッシュを突合する。
//
// mappings は全ワイプしない: Google の「410 時は全ワイプ」公式手順を mappings に
// 適用するとループ防止(一次判定)と origin→blocker の対応関係が失われるため、
// 破棄するのはカーソルと、フル結果に現れなくなったキャッシュ行のみ(仕様8章の確定判断)。
// カーソル失効時(SyncCalendar からの委譲)と日次リコンサイル(Task 11)の両方で使う。
func (e *Engine) FullResync(ctx context.Context, ref model.CalendarRef) error {
	if err := e.Store.UpsertCalendar(ref); err != nil {
		return err
	}
	p, err := e.providerFor(ref.AccountID)
	if err != nil {
		return err
	}
	w := e.currentWindow()
	events, newCursor, err := p.Changes(ctx, ref, "", w)
	if err != nil {
		return err
	}

	// フル結果のうち「キャッシュに残るべきイベント」(busy・未辞退・ウィンドウ内)の ID 集合。
	// ウィンドウ外・非 busy 化・削除済みはここに入らず、下の消滅処理で掃除される。
	alive := make(map[string]bool, len(events))
	for _, ev := range events {
		if ev.Deleted || !ShouldBlock(ev) || !InWindow(w, ev) {
			continue
		}
		// ループ遮断を alive にも適用する: 自作ブロッカー(タグ or mappings 一次判定)は
		// origin ではないため「生きているキャッシュ対象」に数えない。ここで除外しないと、
		// 事故等で誤キャッシュされたブロッカー行と、それを origin とする汚染 mappings が
		// set-difference を永遠に免れて残存する(実障害 2026-07-04 の残存経路)。
		if ev.OriginTag != "" {
			continue
		}
		known, kerr := e.Store.IsBlocker(ref.AccountID, ev.ID)
		if kerr != nil {
			return kerr
		}
		if known {
			continue
		}
		alive[ev.ID] = true
	}

	// set-difference: キャッシュにあるがフル結果で生きていない → 消滅扱い。
	// ブロッカー削除 + mapping 削除 + キャッシュ削除 + suppressed 昇格
	// (processEvent の削除通知処理と同じ手順。キャッシュ由来の ID なので未知IDガードは不要)
	// ターゲットの認証失効(TargetAuthError のみのエラー)はそのイベントの後続処理を
	// スキップして続行する(仕様9.3: 失効ターゲットが origin の同期を止めない)
	var errs []error
	cachedIDs, err := e.Store.ListEventIDs(ref)
	if err != nil {
		return err
	}
	for _, id := range cachedIDs {
		if alive[id] {
			continue
		}
		cached, err := e.Store.GetEvent(ref, id)
		if err != nil {
			return err
		}
		if err := e.deleteBlockersForOrigin(ctx, ref, id); err != nil {
			errs = append(errs, err)
			if onlyTargetAuthErrors(err) {
				continue // mapping ごと残る。復帰後のリコンサイルで再削除される
			}
			return errors.Join(errs...)
		}
		if err := e.Store.DeleteEvent(ref, id); err != nil {
			errs = append(errs, err)
			return errors.Join(errs...)
		}
		if cached != nil && cached.ICalUID != "" {
			if err := e.promoteSuppressed(ctx, ref.AccountID, cached.ICalUID); err != nil {
				errs = append(errs, err)
				if onlyTargetAuthErrors(err) {
					continue
				}
				return errors.Join(errs...)
			}
		}
	}

	// フル結果を通常の決定則で処理する(新規範囲入りの作成・time_hash 不一致の patch・
	// pending 行の解決は processEvent / upsertBlockers 側の既存ロジックが行う)
	for _, ev := range events {
		if err := e.processEvent(ctx, ref, ev); err != nil {
			errs = append(errs, err)
			if onlyTargetAuthErrors(err) {
				continue
			}
			return errors.Join(errs...)
		}
	}

	// 完走した時だけ新カーソルと新ウィンドウを永続化する(仕様5.1のカーソル規律と同じ)。
	// TargetAuthError のみの場合は SyncCalendar と同じ理由でカーソルを前進させる
	if newCursor != "" {
		if err := e.Store.SetCursor(ref, newCursor, w); err != nil {
			errs = append(errs, err)
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

// Reconcile はフルリコンサイル(仕様8章)。日次スケジュール(Task 18)と
// `calsync reconcile`(Task 19)から呼ばれる。
// フェーズ: (1) 全監視カレンダーの FullResync(カーソル張り直し + set-difference)
// (2) pending 解決 (3) adoption(孤児ブロッカーの収容/掃除)
// (4) 手で消されたブロッカーの再作成 (5) suppressed 再評価。
// pending 解決を adoption より先に行うのは、pending 行に紐づく作成済みブロッカーを
// adoption が孤児と誤認して掃除しないため。restoreMissingBlockers を adoption の
// 後に置くのは、adoption が同じ回で active 化した行(遅れて実在確認できた分)も
// 復元対象の判定に含めるため。
// 1 カレンダーの障害で全体を止めず、エラーは集約して返す(仕様10章)。
func (e *Engine) Reconcile(ctx context.Context) error {
	var errs []error
	// フェーズ0: タグからの mappings 先行再構築(削除判断は一切しない)。
	// DB 全損直後は mappings が空で、Graph delta はタグを返せないため、これを
	// 配布(FullResync)より先に行わないと Microsoft カレンダー上の受領ブロッカーが
	// 実予定と誤認され全カレンダーへ再ミラーされる(実障害 2026-07-04: 複製957件)。
	if err := e.rebuildMappingsFromTags(ctx); err != nil {
		errs = append(errs, fmt.Errorf("rebuild mappings from tags: %w", err))
	}
	healthy := make(map[model.CalendarRef]bool)
	for _, acct := range e.Cfg.Accounts {
		for _, calID := range acct.Calendars {
			ref := model.CalendarRef{AccountID: acct.ID, CalendarID: calID}
			if err := e.FullResync(ctx, ref); err != nil {
				errs = append(errs, fmt.Errorf("full resync %s: %w", ref, err))
			} else {
				healthy[ref] = true
			}
		}
	}
	// origin 消滅 mapping の掃除は restoreMissingBlockers より前に行う
	// (残すと restore が消滅 origin のブロッカーを再作成し続ける)。
	if err := e.cleanStaleMappings(ctx, healthy); err != nil {
		errs = append(errs, fmt.Errorf("clean stale mappings: %w", err))
	}
	if err := e.resolvePending(ctx); err != nil {
		errs = append(errs, fmt.Errorf("resolve pending: %w", err))
	}
	if err := e.adoptOrphans(ctx); err != nil {
		errs = append(errs, fmt.Errorf("adopt orphans: %w", err))
	}
	if err := e.restoreMissingBlockers(ctx); err != nil {
		errs = append(errs, fmt.Errorf("restore missing blockers: %w", err))
	}
	if err := e.reevaluateSuppressed(ctx); err != nil {
		errs = append(errs, fmt.Errorf("reevaluate suppressed: %w", err))
	}
	// 通知送信記録の掃除(スペック 4.3: start_utc < now-48h。日次リコンサイルは
	// デーモンで常に有効なため、ここへの相乗りでテーブルは肥大しない)
	if err := e.Store.CleanupRemindersSent(e.now().Add(-48 * time.Hour)); err != nil {
		errs = append(errs, fmt.Errorf("cleanup reminders_sent: %w", err))
	}
	return errors.Join(errs...)
}

// resolvePending は pending のまま残った mappings(作成実行と active 更新の間の
// クラッシュ跡)を解決する(仕様6.4)。origin が events キャッシュに生きていれば
// 同一冪等キーで CreateBlocker を再実行する — 既に作成済みなら既存 ID が返るため、
// 実在確認と収容を兼ねる。origin が消えていれば intent ごと破棄する
// (ブロッカーが作られてしまっていた場合は直後の adoption が掃除する)。
// 1 行の失敗で残りを止めず、エラーは行単位で集約して返す(仕様10章。
// 最終ホールブランチレビュー追補 Issue 6)。
func (e *Engine) resolvePending(ctx context.Context) error {
	pend, err := e.Store.ListPendingMappings()
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range pend {
		originRef := model.CalendarRef{AccountID: m.OriginAccount, CalendarID: m.OriginCalendar}
		ev, err := e.Store.GetEvent(originRef, m.OriginEventID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ev == nil {
			if err := e.Store.DeleteMapping(m.OriginAccount, m.OriginCalendar, m.OriginEventID, m.TargetAccount); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := e.createFromMapping(ctx, m, *ev); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rebuildMappingsFromTags は全カレンダーの calsync タグ付きブロッカーを列挙し、
// mappings に存在しないものを active として先行再収容する(フェーズ0)。
// ここでは削除判断を一切しない: origin の生死はこの時点ではイベントキャッシュが
// 空・古い可能性があり判定できないため、掃除は FullResync 後のフェーズに委ねる。
func (e *Engine) rebuildMappingsFromTags(ctx context.Context) error {
	w := e.currentWindow()
	var errs []error
	for _, acct := range e.Cfg.Accounts {
		ref := model.CalendarRef{AccountID: acct.ID, CalendarID: acct.BlockerCalendar}
		p, err := e.providerFor(acct.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("account %s: %w", acct.ID, err))
			continue
		}
		recs, err := p.ListBlockers(ctx, ref, w)
		if err != nil {
			errs = append(errs, fmt.Errorf("list blockers %s: %w", ref, err))
			continue
		}
		for _, rec := range recs {
			known, err := e.Store.IsBlocker(acct.ID, rec.EventID)
			if err != nil {
				errs = append(errs, fmt.Errorf("is blocker %s on %s: %w", rec.EventID, ref, err))
				continue
			}
			if known {
				continue
			}
			originAcct, originEventID, ok := parseOriginTag(rec.OriginTag)
			if !ok {
				continue // calsync 製と断定できないため触らない
			}
			// origin のカレンダーはタグに含まれない。監視対象の先頭(v1 は実質 primary)
			// を採用する。実際の配置は後続フェーズの突合で収斂する。
			originCal := "primary"
			if oa := e.Cfg.AccountByID(originAcct); oa != nil && len(oa.Calendars) > 0 {
				originCal = oa.Calendars[0]
			}
			existing, err := e.Store.GetMapping(originAcct, originCal, originEventID, acct.ID)
			if err != nil {
				errs = append(errs, fmt.Errorf("get mapping for %s on %s: %w", rec.EventID, ref, err))
				continue
			}
			if existing != nil {
				continue // 同一 origin/target 対が既に存在(pending/suppressed 含め尊重)
			}
			m := store.Mapping{
				OriginAccount:  originAcct,
				OriginCalendar: originCal,
				OriginEventID:  originEventID,
				TargetAccount:  acct.ID,
				TargetCalendar: acct.BlockerCalendar,
				BlockerEventID: rec.EventID,
				IdempotencyKey: idemKeyFor(acct.Provider, rec.OriginTag, acct.ID),
				TimeHash:       e.policyHashFor(rec.TimeHash, acct.ID) + detailHashSentinel,
				Status:         store.StatusActive,
			}
			if err := e.Store.PutMapping(m); err != nil {
				errs = append(errs, fmt.Errorf("adopt %s on %s: %w", rec.EventID, ref, err))
			}
		}
	}
	return errors.Join(errs...)
}

// cleanStaleMappings は「フル同期が成功したカレンダーの origin なのに、イベント
// キャッシュに origin が存在しない active mapping」を blocker ごと掃除する。
// フル同期成功後のキャッシュはそのカレンダーの正であり、そこに無い origin は
// 消滅している(または実予定ではない = 汚染 mapping)。失敗したカレンダーの
// origin には触らない(キャッシュが古い可能性があるため安全側)。
func (e *Engine) cleanStaleMappings(ctx context.Context, healthy map[model.CalendarRef]bool) error {
	var errs []error
	done := make(map[string]bool) // origin 単位の重複処理防止
	for _, acct := range e.Cfg.Accounts {
		maps, err := e.Store.ListMappingsWhereOriginAccount(acct.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("list mappings for %s: %w", acct.ID, err))
			continue
		}
		for _, m := range maps {
			if m.Status != store.StatusActive {
				continue
			}
			oref := model.CalendarRef{AccountID: m.OriginAccount, CalendarID: m.OriginCalendar}
			if !healthy[oref] {
				continue
			}
			key := oref.String() + "|" + m.OriginEventID
			if done[key] {
				continue
			}
			ev, err := e.Store.GetEvent(oref, m.OriginEventID)
			if err != nil {
				errs = append(errs, fmt.Errorf("get event %s: %w", key, err))
				continue
			}
			if ev != nil {
				continue // origin 生存
			}
			done[key] = true
			if err := e.deleteBlockersForOrigin(ctx, oref, m.OriginEventID); err != nil {
				errs = append(errs, fmt.Errorf("clean stale origin %s: %w", key, err))
			}
		}
	}
	return errors.Join(errs...)
}

// adoptOrphans は各アカウントのブロッカー書き込み先を ListBlockers(タグ検索)で
// 列挙し、mappings 未登録のタグ付きブロッカー(クラッシュ起因・DB 消失起因の孤児)を
// origin の実在に応じて収容 or 削除する(仕様8章 3)。DB 全損時の mappings 再構築も
// この経路で行われる(仕様8章 5)。origin の実在は直前の FullResync で最新化された
// events キャッシュで判定する。
// 1 アカウント/1 ブロッカーの失敗で残りを止めず、エラーは集約して返す(仕様10章。
// 最終ホールブランチレビュー追補 Issue 6)。
func (e *Engine) adoptOrphans(ctx context.Context) error {
	w := e.currentWindow()
	var errs []error
	for _, acct := range e.Cfg.Accounts {
		ref := model.CalendarRef{AccountID: acct.ID, CalendarID: acct.BlockerCalendar}
		p, err := e.providerFor(acct.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("account %s: %w", acct.ID, err))
			continue
		}
		recs, err := p.ListBlockers(ctx, ref, w)
		if err != nil {
			errs = append(errs, fmt.Errorf("list blockers %s: %w", ref, err))
			continue
		}
		for _, rec := range recs {
			if err := e.adoptOrphan(ctx, p, acct, ref, rec); err != nil {
				errs = append(errs, fmt.Errorf("adopt %s on %s: %w", rec.EventID, ref, err))
			}
		}
	}
	return errors.Join(errs...)
}

// adoptOrphan は 1 ブロッカー分の孤児判定・収容・掃除を行う(adoptOrphans の 1 反復)。
func (e *Engine) adoptOrphan(ctx context.Context, p provider.Provider, acct config.Account, ref model.CalendarRef, rec model.BlockerRecord) error {
	known, err := e.Store.IsBlocker(acct.ID, rec.EventID)
	if err != nil {
		return err
	}
	if known {
		return nil // mappings 登録済みの正規ブロッカー
	}
	originAcct, originEventID, ok := parseOriginTag(rec.OriginTag)
	if !ok {
		return nil // タグ形式不正。calsync 製と断定できないため触らない
	}
	ev, originCal, err := e.findCachedOriginEvent(originAcct, originEventID)
	if err != nil {
		return err
	}
	if ev == nil {
		// origin 消滅(またはアカウントが監視対象外)→ 掃除
		return p.DeleteBlocker(ctx, ref, rec.EventID)
	}
	existing, err := e.Store.GetMapping(originAcct, originCal, originEventID, acct.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.BlockerEventID != rec.EventID {
		// 同じ origin/target 対に別のブロッカー(または suppressed/pending の意図)が
		// 既に紐づいている → この孤児は重複。掃除
		return p.DeleteBlocker(ctx, ref, rec.EventID)
	}
	// origin 生存 → active として収容(削除・再作成はしない)
	m := store.Mapping{
		OriginAccount:  originAcct,
		OriginCalendar: originCal,
		OriginEventID:  originEventID,
		TargetAccount:  acct.ID,
		TargetCalendar: acct.BlockerCalendar,
		BlockerEventID: rec.EventID,
		IdempotencyKey: idemKeyFor(acct.Provider, rec.OriginTag, acct.ID),
		TimeHash:       e.policyHashFor(rec.TimeHash, acct.ID) + detailHashSentinel,
		Status:         store.StatusActive,
	}
	return e.Store.PutMapping(m)
}

// restoreMissingBlockers は「手で消されたブロッカーの再作成」を行う(仕様8章4:
// 元予定が生きている限りブロッカーは維持する、確定仕様)。processEvent の
// time_hash 一致判定(upsertBlockers の default ケース)はプロバイダを一切
// 呼ばないため、ブロッカー本体だけが手動削除されても通常の同期では検知できない
// (最終ホールブランチレビュー所見2)。
//
// 各アカウントのブロッカー書き込み先を ListBlockers で列挙して実在 ID 集合を作り、
// そのアカウントを target とする active mapping のうち BlockerEventID がその集合に
// 無い行を、origin イベントがキャッシュに残っていれば createFromMapping(既存の
// pending→CreateBlocker→active の流れ)で再作成する。origin がキャッシュに無ければ
// スキップする(次回 FullResync が整合を回復する)。
// 実プロバイダでは決定的な冪等キーでの再送が 409 → cancelled 蘇生の経路を通るため
// (修正1)、fake のようにキーを解放して新規作成になる意味論・実プロバイダのように
// 蘇生になる意味論のどちらでも成立する。
// 1 アカウント/1 行の失敗で残りを止めず、エラーは集約して返す(仕様10章)。
func (e *Engine) restoreMissingBlockers(ctx context.Context) error {
	w := e.currentWindow()
	var errs []error
	for _, acct := range e.Cfg.Accounts {
		ref := model.CalendarRef{AccountID: acct.ID, CalendarID: acct.BlockerCalendar}
		p, err := e.providerFor(acct.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("account %s: %w", acct.ID, err))
			continue
		}
		recs, err := p.ListBlockers(ctx, ref, w)
		if err != nil {
			errs = append(errs, fmt.Errorf("list blockers %s: %w", ref, err))
			continue
		}
		existing := make(map[string]bool, len(recs))
		for _, rec := range recs {
			existing[rec.EventID] = true
		}

		maps, err := e.Store.ListMappingsWhereTargetAccount(acct.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, m := range maps {
			if m.Status != store.StatusActive || existing[m.BlockerEventID] {
				continue
			}
			originRef := model.CalendarRef{AccountID: m.OriginAccount, CalendarID: m.OriginCalendar}
			ev, err := e.Store.GetEvent(originRef, m.OriginEventID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ev == nil {
				continue // origin がキャッシュに無い → 次回 FullResync が整合を回復する
			}
			if err := e.createFromMapping(ctx, m, *ev); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// reevaluateSuppressed は suppressed mappings を再評価する(仕様6.5)。
// 抑止理由だった重複実予定が消えていれば昇格し、origin 自体が消えていれば行を掃除する。
// 削除通知経由の promoteSuppressed(Task 9)を取り逃がした場合のセーフティネット。
// 1 アカウント/1 行の失敗で残りを止めず、エラーは集約して返す(仕様10章)。
func (e *Engine) reevaluateSuppressed(ctx context.Context) error {
	var errs []error
	for _, acct := range e.Cfg.Accounts {
		maps, err := e.Store.ListMappingsWhereTargetAccount(acct.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, m := range maps {
			if m.Status != store.StatusSuppressed {
				continue
			}
			originRef := model.CalendarRef{AccountID: m.OriginAccount, CalendarID: m.OriginCalendar}
			ev, err := e.Store.GetEvent(originRef, m.OriginEventID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ev == nil {
				// origin 消滅 → suppressed 行を掃除(ブロッカーは元々存在しない)
				if err := e.Store.DeleteMapping(m.OriginAccount, m.OriginCalendar, m.OriginEventID, m.TargetAccount); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			target := e.Cfg.AccountByID(m.TargetAccount)
			if target == nil {
				continue
			}
			dup, err := e.isDuplicateOnTarget(*target, *ev)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if dup {
				continue // 重複実予定が健在 → 抑止継続
			}
			if err := e.createFromMapping(ctx, m, *ev); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// parseOriginTag は "<origin_account_id>:<origin_event_id>" を分解する。
// イベント ID には ":" が含まれうるため最初の ":" で切る
// (アカウント ID に ":" を含めない前提。model.OriginTagOf と対)。
func parseOriginTag(tag string) (accountID, eventID string, ok bool) {
	accountID, eventID, ok = strings.Cut(tag, ":")
	if !ok || accountID == "" || eventID == "" {
		return "", "", false
	}
	return accountID, eventID, true
}

// findCachedOriginEvent は origin アカウントの監視カレンダーの events キャッシュから
// origin イベントを探す(タグにはカレンダー ID が含まれないため全監視カレンダーを見る)。
// 見つかればイベントとカレンダー ID を、アカウントが設定に無い/キャッシュに無ければ
// (nil, "", nil) を返す。
func (e *Engine) findCachedOriginEvent(originAccountID, originEventID string) (*model.NormalizedEvent, string, error) {
	acct := e.Cfg.AccountByID(originAccountID)
	if acct == nil {
		return nil, "", nil
	}
	for _, calID := range acct.Calendars {
		ref := model.CalendarRef{AccountID: originAccountID, CalendarID: calID}
		ev, err := e.Store.GetEvent(ref, originEventID)
		if err != nil {
			return nil, "", err
		}
		if ev != nil {
			return ev, calID, nil
		}
	}
	return nil, "", nil
}
