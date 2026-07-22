package appserver

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maintenanceTimeout は bootout → reconcile → bootstrap 全体に許すコンテキスト
// 予算(仕様書 §4)。reconcile サブプロセスが長時間かかるケース(初回の
// FullResync 等)を想定して長めに確保している。
const maintenanceTimeout = 30 * time.Minute

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
// を常にこの順で実行する。各ステップの失敗は次のステップの実行を妨げない
// (仕様書の「(3) 成否に関わらず bootstrap で再開」を、bootout の失敗にも
// 素直に広げたもの: bootout が失敗してもデーモンがまだ動いている可能性がある
// 状態で reconcile を試すことになるが、store.Open の flock が二重起動を弾く
// ため実データ破損には至らず、reconcile 側のエラーとして安全に失敗する)。
func (s *Server) runMaintenanceWindow(logPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), maintenanceTimeout)
	defer cancel()

	target := fmt.Sprintf("gui/%d/%s", s.UID, launchdLabel)
	var errs []string

	if _, stderr, err := s.Runner.Run(ctx, "launchctl", "bootout", target); err != nil {
		errs = append(errs, "bootout: "+launchctlErrMessage(stderr, err))
	}

	run := s.RunReconcile
	if run == nil {
		run = s.defaultRunReconcile
	}
	if err := run(ctx, logPath); err != nil {
		errs = append(errs, "reconcile: "+err.Error())
	}

	if _, stderr, err := s.Runner.Run(ctx, "launchctl", "bootstrap", fmt.Sprintf("gui/%d", s.UID), s.PlistPath); err != nil {
		errs = append(errs, "bootstrap: "+launchctlErrMessage(stderr, err))
	}

	s.maintSt.mu.Lock()
	defer s.maintSt.mu.Unlock()
	if len(errs) > 0 {
		s.maintSt.phase = "error"
		s.maintSt.errMsg = strings.Join(errs, "; ")
		return
	}
	s.maintSt.phase = "done"
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
