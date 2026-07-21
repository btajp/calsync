package appserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/btajp/calsync/internal/config"
)

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
