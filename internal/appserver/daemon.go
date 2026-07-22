package appserver

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// CmdRunner は外部コマンド実行を抽象化する(launchctl / docker)。テストでは
// fake 実装に差し替えて台本どおりの出力を返させる。
type CmdRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// execRunner は CmdRunner の既定実装(実プロセス起動)。
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	var so, se bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	return so.String(), se.String(), err
}

const launchdLabel = "com.btajp.calsync"

// DaemonInfo は運用形態の判定結果。
type DaemonInfo struct {
	Mode    string `json:"mode"` // "launchd" | "manual" | "container" | "unknown"
	Running bool   `json:"running"`
	Detail  string `json:"detail,omitempty"`
}

// detectDaemon は運用形態を判定する。
// plist あり → launchd 管理(launchctl print で稼働判定)。ただし print が失敗
// (未ロード = installed but not loaded)する場合は、plist が古い残骸で実運用は
// docker 版という構成もあり得るため、先に container 判定を試す(最終ホール
// レビュー Fix 2: stale plist + コンテナ稼働でホストが DB 読み取りに到達するのを防ぐ)。
// plist なし → docker で calsync コンテナ稼働中なら container(全 DB アクセス禁止)、
// でなければ manual(手動運用 or 未セットアップ。DB には触らない)。
func (s *Server) detectDaemon(ctx context.Context) DaemonInfo {
	if _, err := os.Stat(s.PlistPath); err == nil {
		target := fmt.Sprintf("gui/%d/%s", s.UID, launchdLabel)
		out, _, err := s.Runner.Run(ctx, "launchctl", "print", target)
		if err != nil {
			if info, ok := s.detectContainer(ctx); ok {
				return info
			}
			return DaemonInfo{Mode: "launchd", Running: false, Detail: "installed but not loaded"}
		}
		return DaemonInfo{Mode: "launchd", Running: strings.Contains(out, "state = running")}
	}
	if info, ok := s.detectContainer(ctx); ok {
		return info
	}
	return DaemonInfo{Mode: "manual", Running: false}
}

// detectContainer は docker CLI 越しに calsync コンテナが稼働中かを判定する。
// docker が PATH に無い・ps が失敗する・calsync という名前のコンテナが無い、の
// いずれでも ok=false を返す(呼び出し側はフォールバック判定を続ける)。
func (s *Server) detectContainer(ctx context.Context) (DaemonInfo, bool) {
	if _, err := s.LookPath("docker"); err != nil {
		return DaemonInfo{}, false
	}
	out, _, err := s.Runner.Run(ctx, "docker", "ps", "--format", "{{.Names}}")
	if err != nil {
		return DaemonInfo{}, false
	}
	for _, name := range strings.Fields(out) {
		if name == "calsync" {
			return DaemonInfo{Mode: "container", Running: true,
				Detail: "container detected: host-side access to data/ is unsafe (VirtioFS)"}, true
		}
	}
	return DaemonInfo{}, false
}

// requireNotContainer は書き込み/認可系エンドポイント(config PUT・auth
// start・calendars)の入口ガード。コンテナ運用のホストからの変更はコンテナ内
// デーモンとの競合(トークンローテーション等)を招くため、仕様§9に従い一律
// 409 で拒否する。拒否した場合 true を返す(呼び出し側は直ちに return する)。
// manual/launchd/unknown は対象外(初回セットアップの config 編集・アカウント
// 追加を止めないため)。
func (s *Server) requireNotContainer(w http.ResponseWriter, r *http.Request) bool {
	if s.detectDaemon(r.Context()).Mode == "container" {
		writeErr(w, http.StatusConflict, "container_mode",
			"calsync appears to be running in a container on this host",
			"コンテナ稼働中はホスト側からの変更・認可はできません。docker compose の手順(README)を使ってください")
		return true
	}
	return false
}

// handleDaemonAction は POST /api/daemon/{start|stop|restart} を処理する。
// launchd モード外は 409 を返す。成功時は {"ok":true} を返す。
func (s *Server) handleDaemonAction(w http.ResponseWriter, r *http.Request) {
	info := s.detectDaemon(r.Context())
	if info.Mode != "launchd" {
		writeErr(w, http.StatusConflict, "not_launchd",
			"daemon is not managed by launchd on this host",
			"launchd 管理外です。./scripts/macos/install-launchd.sh でのセットアップ、または手動での操作を行ってください")
		return
	}
	target := fmt.Sprintf("gui/%d/%s", s.UID, launchdLabel)
	var args []string
	switch r.PathValue("action") {
	case "start":
		args = []string{"bootstrap", fmt.Sprintf("gui/%d", s.UID), s.PlistPath}
	case "stop":
		args = []string{"bootout", target}
	case "restart":
		args = []string{"kickstart", "-k", target}
	default:
		writeErr(w, http.StatusNotFound, "unknown_action", "unknown daemon action", "")
		return
	}
	if _, stderr, err := s.Runner.Run(r.Context(), "launchctl", args...); err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			// launchctl が stderr に何も書かずに失敗するケース(実行ファイルが
			// 見つからない等)がある。空メッセージのままだと呼び出し元は原因を
			// 知る手掛かりを一切得られないため、err.Error() にフォールバックする。
			msg = err.Error()
		}
		writeErr(w, http.StatusBadGateway, "launchctl_failed", msg, "launchctl の失敗です。ログを確認してください")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
