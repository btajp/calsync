import { useState } from "react";
import { open } from "@tauri-apps/plugin-shell";
import { isHttpsUrl } from "../urlSafety";
import type { EventOut } from "../types";

export interface DescriptionSegment {
  type: "text" | "link";
  value: string;
}

// http/https のみを URL として抽出する(デスクトップ予定詳細設計 2026-07-24 §3.1・§4)。
// mailto: 等の他スキームはリンク化しない。文字クラスは RFC 3986 の URL 構成文字に限定する
// ([^\s]+ だと「詳細はhttps://example.com/docを参照」のような日本語説明文で URL 直後の
// 地の文まで丸ごと URL に取り込んでしまう)。日本語・全角約物・<> はクラス外なので自然に
// 終端する。IPv6 ホストの [] は説明文では実質使われず、末尾 ] の誤取り込みの害が大きい
// ため含めない。
const URL_PATTERN = /https?:\/\/[A-Za-z0-9\-._~:/?#@!$&'()*+,;=%]+/g;

/**
 * URL 末尾に食い込みがちな半角約物を落とす純関数。「URL, 続き」「(URL)」のような
 * 文中利用で末尾の , . ) 等が URL に含まれてしまうのを防ぐ。閉じ丸括弧は URL 内で
 * 対応が取れている場合のみ残す(Wikipedia 等の「/wiki/Foo_(bar)」を壊さない)。
 */
export function trimUrlTail(url: string): string {
  let u = url;
  for (;;) {
    const last = u[u.length - 1];
    if (last === ")") {
      const opens = (u.match(/\(/g) ?? []).length;
      const closes = (u.match(/\)/g) ?? []).length;
      if (closes > opens) {
        u = u.slice(0, -1);
        continue;
      }
      break;
    }
    if (last && ".,;:!?'".includes(last)) {
      u = u.slice(0, -1);
      continue;
    }
    break;
  }
  return u;
}

/**
 * 説明文(プレーンテキスト)を「地の文」と「URL」に分割する純関数。
 * 改行はそのまま value に含めて返す(呼び出し側が white-space: pre-wrap で表示する)。
 */
export function linkifyDescription(text: string): DescriptionSegment[] {
  if (!text) return [];
  const segments: DescriptionSegment[] = [];
  let lastIndex = 0;
  for (const match of text.matchAll(URL_PATTERN)) {
    const start = match.index ?? 0;
    if (start > lastIndex) {
      segments.push({ type: "text", value: text.slice(lastIndex, start) });
    }
    // 末尾トリムで削れた分は次の text セグメントに戻る(lastIndex をトリム後の長さで進める)。
    const url = trimUrlTail(match[0]);
    segments.push({ type: "link", value: url });
    lastIndex = start + url.length;
  }
  if (lastIndex < text.length) {
    segments.push({ type: "text", value: text.slice(lastIndex) });
  }
  return segments;
}

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

/**
 * ローカルの "YYYY-MM-DD" 文字列から Date を作る。`new Date("YYYY-MM-DD")` は UTC 深夜と
 * 解釈されるため TZ によっては前日にずれる(CalendarView/PanelApp と同じ理由で年月日を
 * 分解して構築する)。
 */
function parseLocalDateOnly(yyyyMmDd: string): Date {
  const [y, m, d] = yyyyMmDd.split("-").map(Number);
  return new Date(y, (m || 1) - 1, d || 1);
}

function formatDateOnly(d: Date): string {
  return `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`;
}

function formatTimeOnly(d: Date): string {
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

/**
 * 予定の日時表示を組み立てる純関数(デスクトップ予定詳細設計 2026-07-24 §3.1:
 * 「終日は『終日』・複数日は範囲」)。
 * - 終日・単日: "YYYY/M/D(終日)"
 * - 終日・複数日: "YYYY/M/D 〜 YYYY/M/D(終日)"(all_day_end は排他的終了日なので
 *   表示は 1 日引いた末日にする。toFullCalendarEvents と同じ規約)
 * - 時刻あり・同日: "YYYY/M/D HH:MM 〜 HH:MM"
 * - 時刻あり・複数日: "YYYY/M/D HH:MM 〜 YYYY/M/D HH:MM"
 */
export function formatEventDateTime(ev: EventOut): string {
  if (ev.all_day) {
    const start = formatDateOnly(parseLocalDateOnly(ev.all_day_start));
    if (ev.all_day_end) {
      const lastDay = parseLocalDateOnly(ev.all_day_end);
      lastDay.setDate(lastDay.getDate() - 1);
      return `${start} 〜 ${formatDateOnly(lastDay)}(終日)`;
    }
    return `${start}(終日)`;
  }
  const start = new Date(ev.start);
  const end = new Date(ev.end);
  if (formatDateOnly(start) === formatDateOnly(end)) {
    return `${formatDateOnly(start)} ${formatTimeOnly(start)} 〜 ${formatTimeOnly(end)}`;
  }
  return `${formatDateOnly(start)} ${formatTimeOnly(start)} 〜 ${formatDateOnly(end)} ${formatTimeOnly(end)}`;
}

function describeOpenError(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export interface EventDetailView {
  title: string;
  dateTimeLabel: string;
  accountIds: string[];
  descriptionSegments: DescriptionSegment[];
  showJoinButton: boolean; // meeting_url があり、かつ https のときのみ
  showCalendarLink: boolean; // html_link があり、かつ https のときのみ
}

/**
 * EventOut から EventDetail の表示条件を導出する純関数(deriveMaintenanceBannerView と
 * 同じ「表示判断はコンポーネント外の純関数に切り出す」方針。vitest は DOM 環境を持たない
 * ため、レンダリング結果ではなくこの関数の戻り値で表示条件を検証する)。
 */
export function deriveEventDetailView(event: EventOut): EventDetailView {
  return {
    title: event.title || "(無題)",
    dateTimeLabel: formatEventDateTime(event),
    accountIds: event.account_ids.length > 0 ? event.account_ids : [event.account_id],
    // https として開けない link セグメント(http URL・パース不能な文字列)は text に
    // 降格する。openHttps は https 以外を無言で拒否するため、降格しないと「リンクの
    // 見た目なのにクリックしても何も起きない」要素ができてしまう(会議ボタンが http の
    // とき非表示になるのと同じ方針)。
    descriptionSegments: linkifyDescription(event.description ?? "").map((seg) =>
      seg.type === "link" && !isHttpsUrl(seg.value) ? { type: "text" as const, value: seg.value } : seg,
    ),
    showJoinButton: isHttpsUrl(event.meeting_url),
    showCalendarLink: isHttpsUrl(event.html_link),
  };
}

/**
 * 予定詳細のアプリ内表示部品(デスクトップ予定詳細設計 2026-07-24 §3.1)。カレンダータブの
 * モーダルとメニューバーのポップオーバーの両方が同じビルド成果物を使う。closeLabel で
 * 閉じるボタンの文言だけを呼び出し側に変えられるようにしている
 * (カレンダー: 「閉じる」・ポップオーバー: 「← 戻る」)。
 */
export default function EventDetail({
  event,
  colorOf,
  onClose,
  closeLabel = "閉じる",
}: {
  event: EventOut;
  colorOf: (accountId: string) => string;
  onClose: () => void;
  closeLabel?: string;
}) {
  const [openError, setOpenError] = useState<string | null>(null);
  const view = deriveEventDetailView(event);

  const openHttps = (url: string) => {
    if (!isHttpsUrl(url)) return;
    setOpenError(null);
    open(url).catch((e) => setOpenError(describeOpenError(e)));
  };

  return (
    <div className="event-detail">
      {/* 会議参加はタイトル右側に置く。長い説明文では下部のボタン行までスクロールが
          必要になるため、最頻用アクションを常に見える位置に出す(2026-07-28 実機
          フィードバック)。 */}
      <div className="event-detail-header">
        <h2>{view.title}</h2>
        {view.showJoinButton && (
          <button className="event-detail-join" onClick={() => openHttps(event.meeting_url)}>
            会議に参加
          </button>
        )}
      </div>
      <p className="event-detail-datetime">{view.dateTimeLabel}</p>
      <div className="calendar-legend event-detail-accounts">
        {view.accountIds.map((id) => (
          <span key={id} className="legend-item">
            <span className="legend-chip" style={{ backgroundColor: colorOf(id) }} />
            {id}
          </span>
        ))}
      </div>
      {openError && <p className="error">開けませんでした: {openError}</p>}
      {view.descriptionSegments.length > 0 && (
        <div className="event-detail-description">
          {view.descriptionSegments.map((seg, i) =>
            seg.type === "link" ? (
              <span key={i} className="description-link" onClick={() => openHttps(seg.value)}>
                {seg.value}
              </span>
            ) : (
              <span key={i}>{seg.value}</span>
            ),
          )}
        </div>
      )}
      <div className="button-row">
        {view.showCalendarLink && (
          <button className="link-button" onClick={() => openHttps(event.html_link)}>
            カレンダーで開く
          </button>
        )}
        <button className="link-button" onClick={onClose}>
          {closeLabel}
        </button>
      </div>
    </div>
  );
}
