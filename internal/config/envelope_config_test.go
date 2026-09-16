package config

import (
	"path/filepath"
	"testing"
)

// 包封还原默认开启：客户端只读顶层 choices，不还原就等于给下游塞一个
// "成功但没有内容"的响应。这是 Cline 的固有形状，不是可选优化。
func TestUnwrapDataEnvelopeDefaultsOn(t *testing.T) {
	if !Default().UnwrapDataEnvelope {
		t.Error("Default() 必须开启包封还原")
	}
	// 只配了别的字段的文件不应把它关掉。
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, `{"listen":"127.0.0.1:9999"}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UnwrapDataEnvelope {
		t.Error("文件里没写这个字段时，应保持默认开启")
	}
}

func TestUnwrapDataEnvelopeCanBeDisabled(t *testing.T) {
	dir := t.TempDir()

	t.Run("文件", func(t *testing.T) {
		path := filepath.Join(dir, "off.json")
		writeFile(t, path, `{"unwrap_data_envelope":false}`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.UnwrapDataEnvelope {
			t.Error("文件显式 false 必须生效")
		}
	})

	t.Run("环境变量", func(t *testing.T) {
		t.Setenv("CLINE_PIN_UNWRAP_DATA_ENVELOPE", "false")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.UnwrapDataEnvelope {
			t.Error("环境变量显式 false 必须生效")
		}
	})

	t.Run("环境变量非法值报错", func(t *testing.T) {
		t.Setenv("CLINE_PIN_UNWRAP_DATA_ENVELOPE", "maybe")
		if _, err := Load(""); err == nil {
			t.Error("非法布尔值必须让加载失败")
		}
	})
}
