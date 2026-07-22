package appserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/btajp/calsync/internal/config"
)

// handleConfigGet は設定ファイルを読み、内容と mtime(PUT の楽観ロックに使う
// base_mtime)を返す。ReadFile と mtime 取得の間には TOCTOU の隙間があり、
// 別プロセス(手編集・別タブの PUT)がその隙間で書き換えると、返す mtime が
// 実際に読んだ内容のものではなくなる(次の PUT が誤って通る/誤って conflict
// になる)。これを縮小するため Stat→ReadFile→Stat の順で読み、前後の mtime が
// 一致しなければ 1 回だけ読み直す(それでも不一致なら安全側に倒して 500)。
// リトライは 1 回に限定し、無限ループや過剰なポーリングにはしない。
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	b, mtime, err := readConfigWithMtime(s.ConfigPath)
	if err != nil {
		if errors.Is(err, errConfigMtimeUnstable) {
			writeErr(w, http.StatusInternalServerError, "config_unstable", err.Error(),
				"設定ファイルが読み取り中に変更され続けています。しばらく待って再試行してください")
			return
		}
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	var raw config.Raw
	if err := yaml.Unmarshal(b, &raw); err != nil {
		writeErr(w, http.StatusInternalServerError, "config_parse", err.Error(), "設定ファイルの YAML が壊れています。手で修復してください")
		return
	}
	writeJSON(w, map[string]any{"raw": raw, "mtime": strconv.FormatInt(mtime.UnixNano(), 10)})
}

// errConfigMtimeUnstable は Stat→ReadFile→Stat を 1 回リトライしても mtime が
// 安定しなかった(読み取り中も書き換えが続いている)ときに返る。
var errConfigMtimeUnstable = errors.New("config file mtime did not stabilize while reading")

// statFile/readFileBytes は readConfigWithMtime が使う低レベル I/O のフック
// (既定は os.Stat/os.ReadFile)。テストで mtime が読み取り中に変化するケースを
// 決定的に再現するために差し替え可能にしてある。
var (
	statFile      = os.Stat
	readFileBytes = os.ReadFile
)

// readConfigWithMtime は path を Stat→ReadFile→Stat の順で読み、前後の mtime が
// 一致することを確認してから内容を返す。不一致なら 1 回だけ読み直し、それでも
// 不一致なら errConfigMtimeUnstable を返す。
func readConfigWithMtime(path string) ([]byte, time.Time, error) {
	for attempt := 0; attempt < 2; attempt++ {
		before, err := statFile(path)
		if err != nil {
			return nil, time.Time{}, err
		}
		b, err := readFileBytes(path)
		if err != nil {
			return nil, time.Time{}, err
		}
		after, err := statFile(path)
		if err != nil {
			return nil, time.Time{}, err
		}
		if before.ModTime().Equal(after.ModTime()) {
			return b, after.ModTime(), nil
		}
	}
	return nil, time.Time{}, errConfigMtimeUnstable
}

func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	if s.requireNotContainer(w, r) {
		return
	}
	var body struct {
		Raw       config.Raw `json:"raw"`
		BaseMtime string     `json:"base_mtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), "")
		return
	}
	nanos, err := strconv.ParseInt(body.BaseMtime, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_mtime", "base_mtime must be a unixnano string", "")
		return
	}
	if err := SaveConfig(s.ConfigPath, &body.Raw, time.Unix(0, nanos)); err != nil {
		switch {
		case errors.Is(err, ErrConflict):
			writeErr(w, http.StatusConflict, "conflict", "config file changed on disk", "外部で変更されています。再読み込みしてください")
		default:
			writeErr(w, http.StatusBadRequest, "invalid_config", err.Error(), "")
		}
		return
	}
	fi, err := os.Stat(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_stat", err.Error(), "")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "mtime": strconv.FormatInt(fi.ModTime().UnixNano(), 10)})
}
