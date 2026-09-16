package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Source 是只读取当前生效配置的最小接口。
//
// 代理层只依赖它，从而与「配置从哪来」解耦——文件、环境变量还是运行期 API。
type Source interface {
	Current() *Config
}

// Store 持有当前生效的配置，支持原子替换与文件热重载。
//
// 读路径无锁（atomic.Pointer）：每个请求都要读规则，不能为改配置付出锁竞争。
// 写路径由 mu 串行化，避免并发重载/持久化把文件写坏。
type Store struct {
	path string
	log  *slog.Logger

	mu      sync.Mutex
	modTime time.Time

	current atomic.Pointer[Config]
}

// NewStore 从配置文件（可为空）构造配置存储，并完成首次加载。
//
// 路径指向的文件不存在时**不算错误**：以默认值 + 环境变量启动，并保留该路径，
// 这样用户随后把文件放进去就能被热重载发现。Docker 下首次 `compose up`
// 还没有 config.json 是常态，不该因此起不来。
func NewStore(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{path: strings.TrimSpace(path), log: log}

	cfg, err := Load(s.path)
	switch {
	case err == nil:
	case s.path != "" && errors.Is(err, os.ErrNotExist):
		log.Warn("config file not found, starting from defaults + environment",
			"path", s.path, "hint", "把它创建出来即可被热重载自动发现")
		cfg = Default()
		applyEnv(cfg)
		if err := cfg.Normalize(); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	s.current.Store(cfg)

	if s.path != "" {
		if info, statErr := os.Stat(s.path); statErr == nil {
			s.modTime = info.ModTime()
		}
	}
	return s, nil
}

// Static 返回一个不可变的配置源，供测试与「配置只来自环境变量」的场景使用。
func Static(cfg *Config) *Store {
	if cfg == nil {
		cfg = Default()
	}
	s := &Store{log: slog.Default()}
	s.current.Store(cfg)
	return s
}

// Current 返回当前生效的配置。返回值视为只读，调用方不得修改。
func (s *Store) Current() *Config {
	if s == nil {
		return Default()
	}
	if cfg := s.current.Load(); cfg != nil {
		return cfg
	}
	return Default()
}

// Path 返回配置文件路径，空串表示没有配置文件。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// ReloadIfChanged 按文件修改时间判断是否需要重载，需要则原子替换。
func (s *Store) ReloadIfChanged() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(false)
}

// Reload 强制重载，忽略修改时间。
func (s *Store) Reload() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked(true)
}

func (s *Store) reloadLocked(force bool) (bool, error) {
	if s.path == "" {
		return false, nil
	}
	info, err := os.Stat(s.path)
	if err != nil {
		// 文件暂时不可读（例如正在被替换）时保留当前配置，
		// 否则一次 I/O 抖动就会让代理退回默认规则。
		return false, fmt.Errorf("stat config: %w", err)
	}
	if !force && info.ModTime().Equal(s.modTime) {
		return false, nil
	}

	cfg, err := Load(s.path)
	if err != nil {
		return false, err
	}
	s.current.Store(cfg)
	s.modTime = info.ModTime()
	return true, nil
}

// SetRules 在运行期替换规则表，并尽力把新规则写回配置文件。
//
// 写回是「尽力而为」：容器里配置常以只读方式挂载，写失败不应该让请求失败，
// 因此返回 persisted=false 并把原因交给调用方展示。
func (s *Store) SetRules(rules []Rule) (persisted bool, persistErr error, err error) {
	normalized := make([]Rule, len(rules))
	copy(normalized, rules)
	for i := range normalized {
		if e := normalized[i].Normalize(i); e != nil {
			return false, nil, e
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 浅拷贝即可：只替换 Rules 切片，其余字段（含 map）都是只读共享的。
	next := *s.Current()
	next.Rules = normalized
	s.current.Store(&next)

	if s.path == "" {
		return false, nil, nil
	}
	if err := s.persistRulesLocked(normalized); err != nil {
		return false, err, nil
	}

	// 落盘读的是**磁盘上的最新文件**，而内存快照可能落后于它（外部刚改过、
	// 热重载还没轮到）。此时若把 mtime 标记为"已处理"，那份外部修改就永远
	// 进不了内存：磁盘是新值、内存是旧值。所以写完立刻强制重读一次，
	// 让内存与磁盘对齐，顺便带上刚写进去的新规则。
	if _, reloadErr := s.reloadLocked(true); reloadErr != nil {
		s.log.Warn("config persisted but reload failed; in-memory rules kept",
			"path", s.path, "error", reloadErr)
	}
	return true, nil, nil
}

// persistRulesLocked 只替换配置文件里的 rules 键，其余内容原样保留。
//
// 刻意不做「把生效配置整体写回」：那样会把环境变量的覆盖值（例如
// CLINE_PIN_API_KEY）烤进文件，属于意料之外的副作用。
//
// 用 map[string]json.RawMessage 而不是 map[string]any：后者会把所有数字落到
// float64，改写无关字段里的大整数（2^53+1 会变成 2^53）。
func (s *Store) persistRulesLocked(rules []Rule) error {
	doc := map[string]json.RawMessage{}
	raw, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("config must be a JSON object to persist rules: %w", err)
		}
		if doc == nil {
			// 顶层是 null：不是对象，写回会 panic 到 nil map 上。
			return errors.New("config must be a JSON object to persist rules, got null")
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次写回：文件还不存在，就创建一个只含 rules 的配置。
	default:
		return fmt.Errorf("read config for persist: %w", err)
	}

	encoded, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("encode rules: %w", err)
	}
	doc["rules"] = encoded

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	out = append(out, '\n')

	// 首选原子替换：同目录临时文件 + rename，避免写到一半留下坏配置。
	if _, atomicErr := s.writeAtomic(out); atomicErr == nil {
		return nil
	} else if !isAtomicReplaceUnavailable(atomicErr) {
		// 退路只在"原子替换确实做不到"时启用（目录不可写、只读挂载等）。
		//
		// 必须区分失败原因：临时文件的 Write/Sync 失败（磁盘满、I/O 错误）说明
		// 内容根本写不出去，此时再 O_TRUNC 原地覆盖，会把唯一完好的配置文件
		// 也截断——既没落盘成功，又毁掉了重建的依据。
		return fmt.Errorf("write config: %w", atomicErr)
	} else if inPlaceErr := s.writeInPlace(out); inPlaceErr != nil {
		return fmt.Errorf("replace config: %w (in-place fallback also failed: %v)", atomicErr, inPlaceErr)
	} else {
		// Docker 单文件 bind mount 下，容器内目录属于 root 而进程是 nonroot，
		// rename 需要**目录**可写因而失败，但只要文件本身可写就能原地覆盖。
		// 原地写不保证原子性，是明确的取舍。
		s.log.Warn("config persisted in place (atomic replace unavailable)",
			"path", s.path, "atomic_error", atomicErr.Error())
		return nil
	}
}

// isAtomicReplaceUnavailable 判断原子替换是不是"这个目录做不到"，
// 而不是"这次内容没写成功"。
//
// 只有前者才允许退化为原地覆盖：原地写不具备原子性（写到一半断电会留下
// 半个文件），是明确的取舍，不能因为任何一次写失败就启用。
//
// 注意不要在这里同时列出 syscall.ENOTSUP 与 syscall.EOPNOTSUPP：Linux 上
// 两者是同一个常量，switch 会因重复 case 直接编译失败（Windows 上却是两个
// 不同的值，本地 vet 发现不了）。改用 errors.ErrUnsupported 表达"不支持"，
// syscall.Errno.Is 已经替我们处理了这两个别名。
func isAtomicReplaceUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EACCES, syscall.EPERM, syscall.EROFS, syscall.EXDEV:
			return true
		}
	}
	return errors.Is(err, errors.ErrUnsupported) || errors.Is(err, os.ErrPermission)
}

// writeAtomic 用同目录临时文件 + rename 原子替换配置，返回临时文件名。
func (s *Store) writeAtomic(out []byte) (string, error) {
	// 沿用原文件权限：配置里可能含 admin_token，把 0600 改写成 0644
	// 等于在运维者不知情的情况下降权。
	mode := os.FileMode(0o644)
	if info, err := os.Stat(s.path); err == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return "", fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后这里必然失败，忽略

	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return tmpName, fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return tmpName, fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return tmpName, fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return tmpName, fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return tmpName, err
	}
	return tmpName, nil
}

// writeInPlace 直接覆盖配置文件（截断后写全量），不做 rename。
func (s *Store) writeInPlace(out []byte) error {
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Watch 周期性检查配置文件是否变化并热重载，直到 ctx 结束。
//
// 这让「改配置」在 Docker 下不再需要重建容器——挂载文件的内容变化
// compose 是察觉不到的，而进程只在启动时读过一次配置。
func (s *Store) Watch(ctx context.Context, interval time.Duration) {
	if s == nil || s.path == "" || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 坏配置会在每个轮询周期重复失败。每次都打 WARN 会在几分钟内刷满日志，
	// 反而盖住别的信息；同一个错误只报一次，恢复时再报一次。
	var lastErr string

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := s.ReloadIfChanged()
			if err != nil {
				if msg := err.Error(); msg != lastErr {
					lastErr = msg
					s.log.Warn("config reload failed, keeping previous rules",
						"path", s.path, "error", err,
						"hint", "同一个错误不会重复告警；文件修好后会自动重新加载")
				}
				continue
			}
			if lastErr != "" {
				s.log.Info("config reload recovered", "path", s.path)
				lastErr = ""
			}
			if changed {
				cfg := s.Current()
				s.log.Info("config reloaded",
					"path", s.path, "rules", len(cfg.Rules), "watch_seconds", cfg.WatchSeconds)
			}
		}
	}
}
