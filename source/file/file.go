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

// Source 从单个本地文件读取配置。格式默认由扩展名推断：.json、.yaml、.yml。
type Source struct {
	path   string
	format confwatch.Format // FormatUnknown 表示按扩展名推断
}

// New 创建文件源，格式按扩展名推断。
func New(path string) *Source { return &Source{path: path} }

// NewWithFormat 创建文件源并显式指定格式，用于没有扩展名（例如容器里挂载到
// /etc/app/config 的文件）或扩展名不可信的场合。format 为 FormatUnknown 时
// 等价于 New。
func NewWithFormat(path string, format confwatch.Format) *Source {
	return &Source{path: path, format: format}
}

// Path 返回文件路径。
func (s *Source) Path() string { return s.path }

// Describe 实现 confwatch.Source。
func (s *Source) Describe() string { return "file:" + s.path }

// Load 实现 confwatch.Source。返回的字节切片由本函数新分配，不复用。
func (s *Source) Load(ctx context.Context) (confwatch.Payload, error) {
	if err := ctx.Err(); err != nil {
		return confwatch.Payload{}, err
	}

	format := s.format
	if format == confwatch.FormatUnknown {
		var err error
		format, err = FormatOf(s.path)
		if err != nil {
			return confwatch.Payload{}, err
		}
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
		return confwatch.FormatUnknown, fmt.Errorf("file: cannot infer format from extension %q of %s", ext, path)
	}
}
