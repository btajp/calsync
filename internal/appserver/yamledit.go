package appserver

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/btajp/calsync/internal/config"
)

// ErrConflict は保存直前に読み直したファイルの mtime が呼び出し元の想定と
// 一致しないときに返る(他プロセス/他タブからの変更を上書きしないためのガード)。
var ErrConflict = errors.New("config file changed on disk")

// SaveConfig は raw を YAML 化し、旧ファイルのコメントを移植して path へ
// アトミックに書き戻す。書き込み前に config.Parse で検証し、失敗時はファイル不変。
// baseMtime が現ファイルの ModTime と一致しなければ ErrConflict。
func SaveConfig(path string, raw *config.Raw, baseMtime time.Time) error {
	oldBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.ModTime().Equal(baseMtime) {
		return ErrConflict
	}

	// 新ツリーを組み立て、旧ツリーからコメントを移植する
	var oldRoot yaml.Node
	if err := yaml.Unmarshal(oldBytes, &oldRoot); err != nil {
		return fmt.Errorf("parse existing config: %w", err)
	}
	var newRoot yaml.Node
	nb, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(nb, &newRoot); err != nil {
		return err
	}
	if len(oldRoot.Content) > 0 && len(newRoot.Content) > 0 {
		newRoot.HeadComment = oldRoot.HeadComment
		newRoot.Content[0].HeadComment = oldRoot.Content[0].HeadComment
		mergeComments(oldRoot.Content[0], newRoot.Content[0])
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&newRoot); err != nil {
		return err
	}
	_ = enc.Close()

	// 検証が通った場合のみ書き込む(不変条件: 壊れた設定を書かない)
	if _, err := config.Parse(buf.Bytes(), path); err != nil {
		return err
	}

	if err := os.WriteFile(path+".bak", oldBytes, 0o600); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mergeComments は new 側の各ノードに old 側の対応ノードのコメントを移植する。
func mergeComments(oldN, newN *yaml.Node) {
	if oldN == nil || newN == nil {
		return
	}
	copyComments := func(src, dst *yaml.Node) {
		if dst.HeadComment == "" {
			dst.HeadComment = src.HeadComment
		}
		if dst.LineComment == "" {
			dst.LineComment = src.LineComment
		}
		if dst.FootComment == "" {
			dst.FootComment = src.FootComment
		}
	}
	switch newN.Kind {
	case yaml.MappingNode:
		if oldN.Kind != yaml.MappingNode {
			return
		}
		oldVals := map[string][2]*yaml.Node{}
		for i := 0; i+1 < len(oldN.Content); i += 2 {
			oldVals[oldN.Content[i].Value] = [2]*yaml.Node{oldN.Content[i], oldN.Content[i+1]}
		}
		for i := 0; i+1 < len(newN.Content); i += 2 {
			if pair, ok := oldVals[newN.Content[i].Value]; ok {
				copyComments(pair[0], newN.Content[i])
				copyComments(pair[1], newN.Content[i+1])
				mergeComments(pair[1], newN.Content[i+1])
			}
		}
	case yaml.SequenceNode:
		if oldN.Kind != yaml.SequenceNode {
			return
		}
		// 要素の対応付け: マップ要素なら id または (from,to)、それ以外は位置
		key := func(n *yaml.Node) string {
			if n.Kind != yaml.MappingNode {
				return ""
			}
			var id, from, to string
			for i := 0; i+1 < len(n.Content); i += 2 {
				switch n.Content[i].Value {
				case "id":
					id = n.Content[i+1].Value
				case "from":
					from = n.Content[i+1].Value
				case "to":
					to = n.Content[i+1].Value
				}
			}
			if id != "" {
				return "id:" + id
			}
			if from != "" || to != "" {
				return "pair:" + from + "->" + to
			}
			return ""
		}
		oldByKey := map[string]*yaml.Node{}
		for _, c := range oldN.Content {
			if k := key(c); k != "" {
				oldByKey[k] = c
			}
		}
		for i, c := range newN.Content {
			var src *yaml.Node
			if k := key(c); k != "" {
				src = oldByKey[k]
			} else if i < len(oldN.Content) {
				src = oldN.Content[i]
			}
			if src != nil {
				copyComments(src, c)
				mergeComments(src, c)
			}
		}
	}
}
