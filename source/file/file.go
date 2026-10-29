// Package file 提供基于本地文件的配置源。
//
// Source 不缓存任何内容：每次 Load 都重新读磁盘。文件变化监听由 watcher 子包
// 负责，两者都通过同一个 Manager.Reload 走同一条流水线。
package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brianbrian666/confwatch"
)

// Source 从单个本地文件读取配置。格式由扩展名推断：.json、.yaml、.yml。
type Source struct {
	path string
}

// New 创建文件源。
func New(path string) *Source { return &Source{path: path} }

// Path 返回文件路径。
func (s *Source) Path() string { return s.path }

// Describe 实现 confwatch.Source。
func (s *Source) Describe() string { return "file:" + s.path }

// Load 实现 confwatch.Source。返回的字节切片由本函数新分配，不复用。
func (s *Source) Load(ctx context.Context) (confwatch.Payload, error) {
	if err := ctx.Err(); err != nil {
		return confwatch.Payload{}, err
	}

	format, err := FormatOf(s.path)
	if err != nil {
		return confwatch.Payload{}, err
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return confwatch.Payload{}, fmt.Errorf("read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return confwatch.Payload{}, fmt.Errorf("read %s: file is empty", s.path)
	}

	return confwatch.Payload{Data: data, Format: format}, nil
}

// FormatOf 根据文件扩展名推断配置格式。
func FormatOf(path string) (confwatch.Format, error) {
	ext := filepath.Ext(path)
	switch strings.ToLower(ext) {
	case ".json":
		return confwatch.FormatJSON, nil
	case ".yaml", ".yml":
		return confwatch.FormatYAML, nil
	default:
		return 0, fmt.Errorf("file: cannot infer format from extension %q of %s", ext, path)
	}
}
