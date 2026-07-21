package appserver

import (
	"bytes"
	"context"
	"fmt"
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
// plist あり → launchd 管理(launchctl print で稼働判定)。
// plist なし → docker で calsync コンテナ稼働中なら container(全 DB アクセス禁止)、
// でなければ manual(手動運用 or 未セットアップ。DB には触らない)。
func (s *Server) detectDaemon(ctx context.Context) DaemonInfo {
	if _, err := os.Stat(s.PlistPath); err == nil {
		target := fmt.Sprintf("gui/%d/%s", s.UID, launchdLabel)
		out, _, err := s.Runner.Run(ctx, "launchctl", "print", target)
		if err != nil {
			return DaemonInfo{Mode: "launchd", Running: false, Detail: "installed but not loaded"}
		}
		return DaemonInfo{Mode: "launchd", Running: strings.Contains(out, "state = running")}
	}
	if _, err := s.LookPath("docker"); err == nil {
		out, _, err := s.Runner.Run(ctx, "docker", "ps", "--format", "{{.Names}}")
		if err == nil {
			for _, name := range strings.Fields(out) {
				if name == "calsync" {
					return DaemonInfo{Mode: "container", Running: true,
						Detail: "container detected: host-side access to data/ is unsafe (VirtioFS)"}
				}
			}
		}
	}
	return DaemonInfo{Mode: "manual", Running: false}
}
