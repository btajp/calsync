package appserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// defaultMaintenanceTimeout は reconcile サブプロセスに許す既定の予算(仕様書
// §4)。reconcile サブプロセスが長時間かかるケース(初回の FullResync 等)を
// 想定して長めに確保している。Server.MaintenanceTimeout が 0 以下ならこれを使う。
const defaultMaintenanceTimeout = 30 * time.Minute

// launchctlStepTimeout は bootout/bootstrap 個々の launchctl 呼び出しに許す
// 予算。reconcile の予算(MaintenanceTimeout)とは意図的に独立させている
// (レビュー指摘の Critical: 単一 ctx を 3 ステップで共有すると、reconcile が
// 予算を使い切って ctx が失効した状態で bootstrap を呼ぶことになり、
// exec.CommandContext はプロセスを起動すらせず context deadline exceeded を
// 返す。デーモンが停止したまま残ってしまうため、bootout/bootstrap は常に
// 新鮮な短い予算で実行する)。
const launchctlStepTimeout = 60 * time.Second

// maintenanceState は進行中のメンテナンス窓(reconcile)1本分の状態。authState
// (authflow.go)と同じく、バックグラウンド goroutine とリクエストハンドラの
// 両方から触られるため、フィールドは必ず mu で保護すること。
type maintenanceState struct {
	mu      sync.Mutex
	phase   string // "idle" | "running" | "done" | "error"
	logPath string
	errMsg  string
}

// handleMaintenanceReconcile は POST /api/maintenance/reconcile。稼働中の
// launchd デーモンを (1) bootout → (2) 自バイナリで reconcile をサブプロセス
// 実行(stdout/stderr をログファイルへ保存)→ (3) 成否に関わらず bootstrap で
// 再開、をバックグラウンドで行う(仕様書 §4)。「書き込み系はデーモン停止中
// のみ」という不変条件を、このメンテナンス窓の中で一括して守る。
//
// launchd 管理外(manual/container/unknown)では 409 で拒否する。判定は
// doctor/events と同じ単一チェック(container はここで Mode が "container" に
// なり同じく not_launchd になる。仕様書の「doctor と同じ 409 ガード+container
// ガード」は、この 1 チェックが両方をまとめてカバーしている)。
func (s *Server) handleMaintenanceReconcile(w http.ResponseWriter, r *http.Request) {
	if info := s.detectDaemon(r.Context()); info.Mode != "launchd" {
		writeErr(w, http.StatusConflict, "not_launchd",
			"maintenance reconcile is only available on a launchd-managed setup",
			"launchd 管理外です。./scripts/macos/install-launchd.sh でのセットアップ、または CLI の calsync reconcile を直接使ってください")
		return
	}

	s.maintSt.mu.Lock()
	if s.maintSt.phase == "running" {
		s.maintSt.mu.Unlock()
		writeErr(w, http.StatusConflict, "maintenance_in_progress", "a maintenance reconcile is already running", "")
		return
	}
	logPath := reconcileLogPath(s.DataDir, time.Now())
	s.maintSt.phase, s.maintSt.logPath, s.maintSt.errMsg = "running", logPath, ""
	s.maintSt.mu.Unlock()

	go s.runMaintenanceWindow(logPath)

	// Content-Type は WriteHeader の前に設定する(authflow.go と同じ注意:
	// writeJSON 内の Set は既にヘッダ送出後のため無効になり text/plain になる)。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"ok": true})
}

// runMaintenanceWindow は (1) bootout (2) reconcile サブプロセス (3) bootstrap
// を常にこの順で実行する。bootstrap は単一の defer で保証する: 通常完了・
// bootout/reconcile の失敗・reconcile の context タイムアウト・想定外の
// panic のいずれの経路でこの関数を抜けても必ず 1 回実行される(レビュー
// 指摘の Critical/Important 対応)。bootout が失敗してもデーモンがまだ動いて
// いる可能性がある状態で reconcile を試すことになるが、store.Open の flock
// が二重起動を弾くため実データ破損には至らず、reconcile 側のエラーとして
// 安全に失敗する。
func (s *Server) runMaintenanceWindow(logPath string) {
	timeout := s.MaintenanceTimeout
	if timeout <= 0 {
		timeout = defaultMaintenanceTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var errs []string

	// bootstrap は defer 側だけで実行する(通常経路で明示的に呼ぶと defer と
	// 二重実行になる)。panic からの recover もここで行う: reconcile
	// (Server.RunReconcile は外部から注入可能なフィールドのため、フェイクの
	// 実装ミス等で panic する可能性がある)が panic しても appserver 全体を
	// 落とさず、bootstrap を実行して phase=error に記録する。
	defer func() {
		if rec := recover(); rec != nil {
			errs = append(errs, fmt.Sprintf("panic: %v", rec))
		}
		if err := s.runLaunchctlStep(ctx, "bootstrap", fmt.Sprintf("gui/%d", s.UID), s.PlistPath); err != nil {
			errs = append(errs, "bootstrap: "+err.Error())
		}
		s.finishMaintenanceWindow(errs)
	}()

	target := fmt.Sprintf("gui/%d/%s", s.UID, launchdLabel)
	if err := s.runLaunchctlStep(ctx, "bootout", target); err != nil {
		errs = append(errs, "bootout: "+err.Error())
	}

	run := s.RunReconcile
	if run == nil {
		run = s.defaultRunReconcile
	}
	if err := run(ctx, logPath); err != nil {
		errs = append(errs, "reconcile: "+err.Error())
	}
}

// runLaunchctlStep は launchctl のサブコマンド(bootout/bootstrap)を、parent
// の残り予算がどうであれ独立した launchctlStepTimeout で実行する。
// context.WithoutCancel(parent) で親の Done/Err 伝播を切り離してから改めて
// タイムアウトを設定するため、parent が(reconcile のタイムアウト等で)既に
// 失効していても exec 系の呼び出しは必ず起動できる。
func (s *Server) runLaunchctlStep(parent context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), launchctlStepTimeout)
	defer cancel()
	_, stderr, err := s.Runner.Run(ctx, "launchctl", args...)
	if err != nil {
		return errors.New(launchctlErrMessage(stderr, err))
	}
	return nil
}

// launchctlErrMessage は daemon.go の handleDaemonAction と同じフォールバック
// (F3: stderr が空なら err.Error())。launchctl が stderr に何も書かずに失敗
// する(実行ファイル不在等)ケースでもメッセージを空にしない。
func launchctlErrMessage(stderr string, err error) string {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	return msg
}

// finishMaintenanceWindow はメンテナンス窓の最終状態を確定する(runMaintenanceWindow
// の唯一の出口である defer からのみ呼ぶ)。
func (s *Server) finishMaintenanceWindow(errs []string) {
	s.maintSt.mu.Lock()
	defer s.maintSt.mu.Unlock()
	if len(errs) > 0 {
		s.maintSt.phase = "error"
		s.maintSt.errMsg = strings.Join(errs, "; ")
		return
	}
	s.maintSt.phase = "done"
}

// handleMaintenanceState は GET /api/maintenance/state。authState
// (authflow.go)と同じポーリングパターンで、進行中/完了したメンテナンス窓の
// スナップショットを返す。
func (s *Server) handleMaintenanceState(w http.ResponseWriter, r *http.Request) {
	s.maintSt.mu.Lock()
	resp := map[string]string{
		"phase":    s.maintSt.phase,
		"log_path": s.maintSt.logPath,
		"error":    s.maintSt.errMsg,
	}
	s.maintSt.mu.Unlock()
	writeJSON(w, resp)
}

// reconcileLogPath は reconcile サブプロセスの stdout/stderr 保存先(仕様書
// §4)。RFC3339(UTC)の ":" はファイル名に使えない環境があるため "-" に置換する。
func reconcileLogPath(dataDir string, at time.Time) string {
	ts := strings.ReplaceAll(at.UTC().Format(time.RFC3339), ":", "-")
	return filepath.Join(dataDir, "reconcile-"+ts+".log")
}

// defaultRunReconcile は Server.RunReconcile の既定実装。自バイナリ
// (os.Executable)で `reconcile --config <ConfigPath> --data <DataDir>` を
// サブプロセス実行し、stdout/stderr の両方を logPath へ書き出す(前回の教訓:
// ログを保存していなかったため失敗時に原因調査ができなかった)。
func (s *Server) defaultRunReconcile(ctx context.Context, logPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, exe, "reconcile", "--config", s.ConfigPath, "--data", s.DataDir)
	cmd.Stdout = f
	cmd.Stderr = f
	return cmd.Run()
}
