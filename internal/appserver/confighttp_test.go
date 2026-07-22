package appserver

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btajp/calsync/internal/config"
)

// fakeFileInfo は fs.FileInfo の最小実装(ModTime のみ意味を持つ)。
type fakeFileInfo struct{ mtime time.Time }

func (f fakeFileInfo) Name() string       { return "calsync.yaml" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0o600 }
func (f fakeFileInfo) ModTime() time.Time { return f.mtime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// withFakeConfigIO は statFile/readFileBytes をテスト用のフックに差し替え、
// テスト終了時に既定(os.Stat/os.ReadFile)へ復元する。
func withFakeConfigIO(t *testing.T, stat func(string) (fs.FileInfo, error), read func(string) ([]byte, error)) {
	t.Helper()
	origStat, origRead := statFile, readFileBytes
	statFile, readFileBytes = stat, read
	t.Cleanup(func() { statFile, readFileBytes = origStat, origRead })
}

// TestReadConfigWithMtimeRetriesOnce は F5 の回帰テスト。1 回目の Stat→Stat で
// mtime が一致しなくても、2 回目(リトライ)で安定すれば成功し、その安定した
// mtime を返すこと。
func TestReadConfigWithMtimeRetriesOnce(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(1001, 0) // attempt0: before と after が食い違う(書き換え中)
	t2 := time.Unix(1002, 0) // attempt1: before/after ともにこれで安定
	var statCalls int
	mtimes := []time.Time{t0, t1, t2, t2}
	withFakeConfigIO(t,
		func(string) (fs.FileInfo, error) {
			mt := mtimes[statCalls]
			statCalls++
			return fakeFileInfo{mtime: mt}, nil
		},
		func(string) ([]byte, error) { return []byte("poll_interval: 1m\n"), nil },
	)
	b, mtime, err := readConfigWithMtime("dummy-path")
	if err != nil {
		t.Fatalf("readConfigWithMtime: %v", err)
	}
	if string(b) != "poll_interval: 1m\n" {
		t.Fatalf("content = %q", b)
	}
	if !mtime.Equal(t2) {
		t.Fatalf("mtime = %v, want %v (the stabilized retry mtime)", mtime, t2)
	}
	if statCalls != 4 {
		t.Fatalf("statFile called %d times, want 4 (2 stats per attempt x 2 attempts)", statCalls)
	}
}

// TestConfigGetReturns500WhenMtimeNeverStabilizes は F5 の回帰テスト。リトライ
// してもなお mtime が一致しない場合、内容を信用せず 500 config_unstable を返す
// こと(無限リトライはしない)。
func TestConfigGetReturns500WhenMtimeNeverStabilizes(t *testing.T) {
	s, _ := testServer(t)
	var statCalls int
	withFakeConfigIO(t,
		func(string) (fs.FileInfo, error) {
			statCalls++
			return fakeFileInfo{mtime: time.Unix(int64(statCalls), 0)}, nil // 呼ぶたびに変化
		},
		func(string) ([]byte, error) { return []byte("poll_interval: 1m\n"), nil },
	)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/config", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	var body apiError
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "config_unstable" {
		t.Fatalf("code = %q, want config_unstable", body.Code)
	}
	if statCalls != 4 {
		t.Fatalf("statFile called %d times, want 4 (bounded retry, not unbounded)", statCalls)
	}
}

func TestConfigGetPut(t *testing.T) {
	s, dir := testServer(t)
	// testServer の config を seedYAML で上書き
	p := filepath.Join(dir, "calsync.yaml")
	os.WriteFile(p, []byte(seedYAML), 0o600)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	var got struct {
		Raw   config.Raw `json:"raw"`
		Mtime string     `json:"mtime"`
	}
	get(t, srv, "test-token", "/api/config", &got)
	if got.Raw.PollInterval != "1m" || got.Mtime == "" {
		t.Fatalf("got %+v", got)
	}

	got.Raw.PollInterval = "5m"
	body, _ := json.Marshal(map[string]any{"raw": got.Raw, "base_mtime": got.Mtime})
	req, _ := http.NewRequest("PUT", srv.URL+"/api/config", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Fatalf("put status = %d", res.StatusCode)
	}
	// 古い mtime での再 PUT は 409
	res2Req, _ := http.NewRequest("PUT", srv.URL+"/api/config", bytes.NewReader(body))
	res2Req.Header.Set("Authorization", "Bearer test-token")
	res2, _ := http.DefaultClient.Do(res2Req)
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d", res2.StatusCode)
	}
}
