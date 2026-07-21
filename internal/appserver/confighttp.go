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

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "config_read", err.Error(), "")
		return
	}
	var raw config.Raw
	if err := yaml.Unmarshal(b, &raw); err != nil {
		writeErr(w, http.StatusInternalServerError, "config_parse", err.Error(), "設定ファイルの YAML が壊れています。手で修復してください")
		return
	}
	fi, _ := os.Stat(s.ConfigPath)
	writeJSON(w, map[string]any{"raw": raw, "mtime": strconv.FormatInt(fi.ModTime().UnixNano(), 10)})
}

func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
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
	fi, _ := os.Stat(s.ConfigPath)
	writeJSON(w, map[string]any{"ok": true, "mtime": strconv.FormatInt(fi.ModTime().UnixNano(), 10)})
}
