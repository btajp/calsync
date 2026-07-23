package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// callRecorder はメンテナンス窓の各ステップ(bootout/reconcile/bootstrap)の
// 呼び出し順序を並行安全に記録する。
type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (c *callRecorder) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name)
}

func (c *callRecorder) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// orderRunner は fakeRunner をラップし、メンテナンス窓を構成する launchctl
// サブコマンド(bootout/bootstrap)の呼び出しを callRecorder へ記録してから
// 台本どおりの応答を返す。detectDaemon が独立に呼ぶ "print"(409 判定・
// GET /api/status 等)は対象外(呼び出し順序の検証対象ではないため)。
type orderRunner struct {
	*fakeRunner
	rec *callRecorder
}

func (r *orderRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	if name == "launchctl" && len(args) > 0 && (args[0] == "bootout" || args[0] == "bootstrap") {
		r.rec.add(args[0])
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

// launchdServer(t) は events_test.go 定義のものを再利用する(plist あり・
// launchctl print が state=running を返す launchd 稼働中モード)。各テストは
// bootout/bootstrap の台本を足すため s.Runner をさらに上書きする。

func post(t *testing.T, srv *httptest.Server, token, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// waitMaintenancePhase は GET /api/maintenance/state が phase==want になるまで
// ポーリングする。want 以外の "error" に落ちた場合は即座に失敗させる(タイム
// アウトまで待たせて原因不明のまま終わらせない)。
func waitMaintenancePhase(t *testing.T, srv *httptest.Server, token, want string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		st := map[string]string{}
		get(t, srv, token, "/api/maintenance/state", &st)
		if st["phase"] == want {
			return st
		}
		if st["phase"] == "error" && want != "error" {
			t.Fatalf("phase = error unexpectedly: %+v", st)
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %q, want %q (state=%+v)", st["phase"], want, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func maintenanceScript(uid int, plist string) map[string]struct {
	out string
	err error
} {
	target := fmt.Sprintf("gui/%d/%s", uid, launchdLabel)
	return map[string]struct {
		out string
		err error
	}{
		"launchctl print " + target:                              {out: "state = running\n"},
		"launchctl bootout " + target:                            {out: ""},
		fmt.Sprintf("launchctl bootstrap gui/%d %s", uid, plist): {out: ""},
	}
}

// TestMaintenanceReconcileRejectedOutsideLaunchd は launchd 管理外(手動運用)
// では 409 not_launchd を返すことを確認する(要件1: 「launchd 以外 409」)。
func TestMaintenanceReconcileRejectedOutsideLaunchd(t *testing.T) {
	s, _ := testServer(t) // plist なし → manual モード
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res := post(t, srv, "test-token", "/api/maintenance/reconcile")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
	var body apiError
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "not_launchd" {
		t.Fatalf("code = %q, want not_launchd", body.Code)
	}
}

// TestMaintenanceReconcileRejectedInContainerMode は要件1の「container 409」。
// コンテナ運用のホストからは bootout/bootstrap を含むメンテナンス窓を一切
// 実行させない(稼働中のコンテナ内デーモンとの競合防止)。
func TestMaintenanceReconcileRejectedInContainerMode(t *testing.T) {
	s := containerModeServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res := post(t, srv, "test-token", "/api/maintenance/reconcile")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
	var body apiError
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "not_launchd" {
		t.Fatalf("code = %q, want not_launchd", body.Code)
	}
}

// TestMaintenanceReconcileHappyPathOrderAndState は要件2: bootout → reconcile
// → bootstrap の呼び出し順序と、running → done の phase 遷移・log_path を
// 検証する。
func TestMaintenanceReconcileHappyPathOrderAndState(t *testing.T) {
	s, dir := launchdServer(t)
	rec := &callRecorder{}
	fr := &fakeRunner{outputs: maintenanceScript(s.UID, s.PlistPath)}
	s.Runner = &orderRunner{fakeRunner: fr, rec: rec}

	release := make(chan struct{})
	var gotLogPath string
	s.RunReconcile = func(ctx context.Context, logPath string) error {
		<-release
		gotLogPath = logPath
		rec.add("reconcile")
		return nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res := post(t, srv, "test-token", "/api/maintenance/reconcile")
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", res.StatusCode)
	}

	// bootout 完了後・reconcile がまだ release されていない状態で running
	running := waitMaintenancePhase(t, srv, "test-token", "running")
	if running["log_path"] == "" {
		t.Fatalf("log_path is empty while running")
	}
	if !strings.HasPrefix(running["log_path"], dir) {
		t.Fatalf("log_path = %q, want under %q", running["log_path"], dir)
	}

	close(release)
	final := waitMaintenancePhase(t, srv, "test-token", "done")
	if final["log_path"] == "" {
		t.Fatalf("log_path is empty on done")
	}
	if final["error"] != "" {
		t.Fatalf("error = %q, want empty on done", final["error"])
	}
	if gotLogPath != final["log_path"] {
		t.Fatalf("RunReconcile logPath = %q, state log_path = %q", gotLogPath, final["log_path"])
	}

	got := rec.snapshot()
	want := []string{"bootout", "reconcile", "bootstrap"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestMaintenanceReconcileFailureStillBootstraps は要件3: reconcile サブ
// プロセスが失敗しても bootstrap は必ず呼ばれ(デーモンを止めっぱなしに
// しない)、phase=error にそのエラーが載ることを検証する。
func TestMaintenanceReconcileFailureStillBootstraps(t *testing.T) {
	s, _ := launchdServer(t)
	rec := &callRecorder{}
	fr := &fakeRunner{outputs: maintenanceScript(s.UID, s.PlistPath)}
	s.Runner = &orderRunner{fakeRunner: fr, rec: rec}

	wantErr := errors.New("reconcile subprocess exited with status 1")
	s.RunReconcile = func(ctx context.Context, logPath string) error {
		rec.add("reconcile")
		return wantErr
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if res := post(t, srv, "test-token", "/api/maintenance/reconcile"); res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", res.StatusCode)
	}

	final := waitMaintenancePhase(t, srv, "test-token", "error")
	if !strings.Contains(final["error"], wantErr.Error()) {
		t.Fatalf("error = %q, want to contain %q", final["error"], wantErr.Error())
	}

	got := rec.snapshot()
	want := []string{"bootout", "reconcile", "bootstrap"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v (bootstrap must still run after reconcile failure)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestMaintenanceReconcileDoubleStart409 は要件4: 進行中に再度 POST すると
// 409 maintenance_in_progress を返し、実行中のメンテナンス窓は妨げないこと。
func TestMaintenanceReconcileDoubleStart409(t *testing.T) {
	s, _ := launchdServer(t)
	s.Runner = &fakeRunner{outputs: maintenanceScript(s.UID, s.PlistPath)}

	release := make(chan struct{})
	defer close(release)
	s.RunReconcile = func(ctx context.Context, logPath string) error {
		<-release
		return nil
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if res := post(t, srv, "test-token", "/api/maintenance/reconcile"); res.StatusCode != http.StatusAccepted {
		t.Fatalf("first start = %d, want 202", res.StatusCode)
	}
	res := post(t, srv, "test-token", "/api/maintenance/reconcile")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second start = %d, want 409", res.StatusCode)
	}
	var body apiError
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "maintenance_in_progress" {
		t.Fatalf("code = %q, want maintenance_in_progress", body.Code)
	}
}

// ctxCheckingRunner は fakeRunner をラップし、渡された ctx が既に失効して
// いれば(ctx.Err() != nil)台本を引かずにそのエラーを返す。これは
// exec.CommandContext の実際の挙動(ctx が既に Done ならプロセスを起動すら
// しない。stdlib exec.go の Start 実装)を模しており、bootout/reconcile が
// 呼び出し順で先に走る fakeRunner(ctx を一切見ない)では再現できない
// Critical 回帰(単一 ctx 共有だと reconcile タイムアウト後の bootstrap が
// 起動すらされない)を検証するためだけに使う。
type ctxCheckingRunner struct {
	*fakeRunner
	rec *callRecorder
}

func (r *ctxCheckingRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	if name == "launchctl" && len(args) > 0 && (args[0] == "bootout" || args[0] == "bootstrap") {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		r.rec.add(args[0])
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

// TestMaintenanceReconcileBootstrapsAfterReconcileTimeout はレビュー指摘の
// Critical の回帰テスト。reconcile が MaintenanceTimeout を使い切って ctx が
// 失効しても、bootstrap は(その失効した ctx を共有せず)独立した予算で必ず
// 起動され、phase=error に reconcile のタイムアウトが記録されること。
func TestMaintenanceReconcileBootstrapsAfterReconcileTimeout(t *testing.T) {
	s, _ := launchdServer(t)
	s.MaintenanceTimeout = 30 * time.Millisecond
	rec := &callRecorder{}
	fr := &fakeRunner{outputs: maintenanceScript(s.UID, s.PlistPath)}
	s.Runner = &ctxCheckingRunner{fakeRunner: fr, rec: rec}

	s.RunReconcile = func(ctx context.Context, logPath string) error {
		<-ctx.Done()
		rec.add("reconcile")
		return ctx.Err()
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if res := post(t, srv, "test-token", "/api/maintenance/reconcile"); res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", res.StatusCode)
	}

	final := waitMaintenancePhase(t, srv, "test-token", "error")
	if !strings.Contains(final["error"], "reconcile:") || !strings.Contains(final["error"], context.DeadlineExceeded.Error()) {
		t.Fatalf("error = %q, want to mention reconcile timeout", final["error"])
	}

	got := rec.snapshot()
	want := []string{"bootout", "reconcile", "bootstrap"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v (bootstrap must still start after reconcile timeout, not share its expired ctx)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestMaintenanceReconcilePanicStillBootstrapsAndSetsError はレビュー指摘の
// Important の回帰テスト。RunReconcile が panic しても appserver プロセス
// 全体は落とさず、bootstrap は必ず実行され、phase=error に panic の内容が
// 記録されること。
func TestMaintenanceReconcilePanicStillBootstrapsAndSetsError(t *testing.T) {
	s, _ := launchdServer(t)
	rec := &callRecorder{}
	fr := &fakeRunner{outputs: maintenanceScript(s.UID, s.PlistPath)}
	s.Runner = &orderRunner{fakeRunner: fr, rec: rec}

	s.RunReconcile = func(ctx context.Context, logPath string) error {
		rec.add("reconcile")
		panic("boom: simulated RunReconcile crash")
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if res := post(t, srv, "test-token", "/api/maintenance/reconcile"); res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", res.StatusCode)
	}

	final := waitMaintenancePhase(t, srv, "test-token", "error")
	if !strings.Contains(final["error"], "boom") {
		t.Fatalf("error = %q, want to contain the panic message", final["error"])
	}

	got := rec.snapshot()
	want := []string{"bootout", "reconcile", "bootstrap"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v (bootstrap must still run after a panic)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	// appserver プロセス自体が落ちていないこと(この行に到達している時点で
	// テストプロセスは生きているが、念のため同一サーバへの後続リクエストが
	// 正常応答することも確認する)。
	var status map[string]any
	if res := get(t, srv, "test-token", "/api/status", &status); res.StatusCode != http.StatusOK {
		t.Fatalf("server unresponsive after panic: status = %d", res.StatusCode)
	}
}

// TestReconcileLogPathIsFilenameSafe は仕様書 §4 のログファイル名要件
// (UTC RFC3339 の ":" をファイル名安全な形へ置換)の回帰テスト。
func TestReconcileLogPathIsFilenameSafe(t *testing.T) {
	at := time.Date(2026, 7, 23, 7, 45, 12, 0, time.UTC)
	got := reconcileLogPath("/data", at)
	want := filepath.Join("/data", "reconcile-2026-07-23T07-45-12Z.log")
	if got != want {
		t.Fatalf("reconcileLogPath = %q, want %q", got, want)
	}
	if strings.Contains(filepath.Base(got), ":") {
		t.Fatalf("log path contains ':' which is unsafe on some filesystems: %q", got)
	}
}
