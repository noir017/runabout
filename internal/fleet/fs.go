package fleet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FileInfo 是 Stat 的最小信息集，本机与远端保持一致。
type FileInfo struct {
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
	IsDir   bool
}

// FS 是文件工具的操作面。远端实现经 Runner 执行 stat/cat 等命令。
type FS interface {
	Stat(ctx context.Context, path string) (FileInfo, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile 覆盖写入。远端必须原地覆盖（cat > 文件），保住 inode；
	// mode 只在新建文件时对本地实现有意义。
	WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode) error
	Remove(ctx context.Context, path string) error
	MkdirAll(ctx context.Context, path string) error
}

// LocalFS 直接操作本机文件，写入保持"临时文件 + rename"。
type LocalFS struct{}

func (LocalFS) Stat(_ context.Context, path string) (FileInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Size: st.Size(), Mode: st.Mode().Perm(), ModTime: st.ModTime(), IsDir: st.IsDir()}, nil
}

func (LocalFS) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (LocalFS) WriteFile(_ context.Context, path string, data []byte, mode os.FileMode) error {
	// 没指定权限时保持已有文件的权限；新文件用 0644。
	if mode == 0 {
		mode = 0o644
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	}
	return atomicWrite(path, data, mode)
}

func (LocalFS) Remove(_ context.Context, path string) error { return os.Remove(path) }

func (LocalFS) MkdirAll(_ context.Context, path string) error { return os.MkdirAll(path, 0o755) }

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// RemoteFS 通过 Runner 操作远端文件。
type RemoteFS struct {
	Runner Runner
	Target string
	// MaxBytes 是单次读取上限，超过直接报错而不是截断。
	MaxBytes int
}

func (r *RemoteFS) run(ctx context.Context, command string, stdin string, maxOut int) (Result, error) {
	return r.Runner.Run(ctx, Spec{
		Target:         r.Target,
		Command:        command,
		Stdin:          stdin,
		MaxOutputBytes: maxOut,
	})
}

func (r *RemoteFS) Stat(ctx context.Context, path string) (FileInfo, error) {
	// %f 是原始 mode（十六进制），不带本地化文本，busybox 与 GNU stat 都支持。
	res, err := r.run(ctx, "stat -c '%s|%f|%Y' -- "+QuotePath(path), "", 64<<10)
	if err != nil {
		return FileInfo{}, err
	}
	if res.ExitCode != 0 {
		return FileInfo{}, commandError(res)
	}
	parts := strings.SplitN(strings.TrimSpace(res.Stdout), "|", 3)
	if len(parts) != 3 {
		return FileInfo{}, fmt.Errorf("无法解析 stat 输出: %q", strings.TrimSpace(res.Stdout))
	}
	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return FileInfo{}, fmt.Errorf("无法解析文件大小 %q", parts[0])
	}
	raw, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return FileInfo{}, fmt.Errorf("无法解析文件权限 %q", parts[1])
	}
	mtime, _ := strconv.ParseInt(parts[2], 10, 64)
	return FileInfo{
		Size:    size,
		Mode:    os.FileMode(raw & 0o777),
		ModTime: time.Unix(mtime, 0),
		IsDir:   raw&0o170000 == 0o040000,
	}, nil
}

func (r *RemoteFS) ReadFile(ctx context.Context, path string) ([]byte, error) {
	limit := r.MaxBytes
	if limit <= 0 {
		limit = 4 << 20
	}
	res, err := r.run(ctx, "cat -- "+QuotePath(path), "", limit+1)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, commandError(res)
	}
	if res.Truncated || len(res.Stdout) > limit {
		return nil, fmt.Errorf("文件超过 %d 字节", limit)
	}
	return []byte(res.Stdout), nil
}

func (r *RemoteFS) WriteFile(ctx context.Context, path string, data []byte, _ os.FileMode) error {
	// 必须是 cat > 文件：临时文件加 rename 会换 inode，单文件 bind mount 会失效。
	res, err := r.run(ctx, "cat > "+QuotePath(path), string(data), 64<<10)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return commandError(res)
	}
	return nil
}

func (r *RemoteFS) Remove(ctx context.Context, path string) error {
	res, err := r.run(ctx, "rm -f -- "+QuotePath(path), "", 64<<10)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return commandError(res)
	}
	return nil
}

func (r *RemoteFS) MkdirAll(ctx context.Context, path string) error {
	res, err := r.run(ctx, "mkdir -p -- "+QuotePath(path), "", 64<<10)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return commandError(res)
	}
	return nil
}

// CommandError 是远端命令以非零退出码结束时的错误。
type CommandError struct {
	Code   int
	Stderr string
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("退出码 %d", e.Code)
	}
	return msg
}

func commandError(res Result) error {
	return &CommandError{Code: res.ExitCode, Stderr: res.Stderr}
}
