package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The Windows adapter uses a hidden Python broker and an LPAC CMD child.
// Child tokens and process jobs enforce workspace isolation without a console.
type WindowsConfig struct {
	Pythonw string `json:"pythonw"`
	Helper  string `json:"helper"`
	Spool   string `json:"spool"`
	Shell   string `json:"shell,omitempty"`
	// Host-only path to the account's owner-approved music registration. It is
	// never accepted from Agent/configuration JSON and adds no general mount.
	MusicRegistration string `json:"-"`
}

func (c WindowsConfig) Validate() error {
	for _, p := range []string{c.Pythonw, c.Helper, c.Spool} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsRune(p, 0) {
			return errors.New("Windows桥接需绝对规范路径：pythonw、helper、spool")
		}
	}
	if !strings.EqualFold(filepath.Base(c.Pythonw), "pythonw.exe") {
		return errors.New("隐藏Windows执行必须使用pythonw.exe，不能使用控制台python.exe")
	}
	if c.MusicRegistration != "" && (!filepath.IsAbs(c.MusicRegistration) || filepath.Clean(c.MusicRegistration) != c.MusicRegistration || strings.ContainsRune(c.MusicRegistration, 0)) {
		return errors.New("Windows音乐登记需宿主绝对规范路径")
	}
	return nil
}
func WindowsLocalPath(path string) (string, error) {
	if runtime.GOOS == "windows" {
		if filepath.IsAbs(path) {
			return filepath.Clean(path), nil
		}
	}
	if len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) {
		relative := filepath.Clean("/" + strings.ReplaceAll(path[3:], `\`, "/"))
		return filepath.Join("/mnt/"+strings.ToLower(path[:1]), strings.TrimPrefix(relative, "/")), nil
	}
	if strings.HasPrefix(path, "/mnt/") && len(path) >= 6 && ((path[5] >= 'a' && path[5] <= 'z') || (path[5] >= 'A' && path[5] <= 'Z')) && (len(path) == 6 || path[6] == '/') {
		clean := filepath.Clean(path)
		base := path[:6]
		if clean != base && !strings.HasPrefix(clean, base+"/") {
			return "", errors.New("映射路径不能越过当前Windows盘符根目录")
		}
		return clean, nil
	}
	return "", errors.New("Windows工作目录应为C:\\或E:\\等盘符绝对路径，或对应/mnt/c、/mnt/e路径")
}
func windowsPath(path string) (string, error) {
	if runtime.GOOS == "windows" {
		return filepath.Clean(path), nil
	}
	p, err := WindowsLocalPath(path)
	if err != nil {
		return "", err
	}
	if len(p) < 6 {
		return "", errors.New("Windows映射路径无效")
	}
	return strings.ToUpper(p[5:6]) + ":\\" + strings.ReplaceAll(strings.TrimPrefix(p[6:], "/"), "/", "\\"), nil
}

type windowsExitError struct{ code int }

func (e *windowsExitError) Error() string { return fmt.Sprintf("Windows命令退出码%d", e.code) }
func runWindowsHidden(ctx context.Context, cfg Config, id, path, command string, out io.Writer) error {
	return runWindowsRequest(ctx, cfg, id, path, command, nil, out)
}

// Fixed broker argv never passes through CMD or PowerShell quoting.
func runWindowsRequest(ctx context.Context, cfg Config, id, path, command string, argv []string, out io.Writer) error {
	return runWindowsPolicyRequest(ctx, cfg, id, path, command, argv, "", nil, out, nil)
}

func runWindowsPolicyRequest(ctx context.Context, cfg Config, id, path, command string, argv []string, policy string, environment map[string]string, out io.Writer, diagnostic io.Writer) error {
	if cfg.Isolation == nil {
		return errors.New("sandbox_required")
	}
	w := cfg.Windows
	if w == nil {
		return errors.New("Windows桥接未配置")
	}
	for _, path := range []string{w.Pythonw, w.Helper} {
		if stat, e := os.Stat(path); e != nil || !stat.Mode().IsRegular() {
			return errors.New("Windows隐藏执行器或脚本不存在；程序未启动")
		}
	}
	// Spool is control-plane data and intentionally outside Agent mounts.
	var e error
	if e = os.MkdirAll(w.Spool, 0700); e != nil {
		return errors.New("Windows桥接暂存目录不可写")
	}
	winDir, e := windowsPath(path)
	if e != nil {
		return e
	}
	dir := filepath.Join(w.Spool, id)
	if e = os.Mkdir(dir, 0700); e != nil {
		return errors.New("Windows执行记录已存在，未重放")
	}
	request := filepath.Join(dir, "request.json")
	requestWin, e := windowsPath(request)
	if e != nil {
		return e
	}
	shell := w.Shell
	if shell == "" {
		shell = `C:\Windows\System32\cmd.exe`
	}
	mounts := []string{}
	readonly := []string{}
	for _, base := range cfg.Roots {
		v, err := windowsPath(base)
		if err != nil {
			return err
		}
		mounts = append(mounts, v)
	}
	for _, base := range cfg.Isolation.ReadOnly {
		v, err := windowsPath(base)
		if err != nil {
			return err
		}
		readonly = append(readonly, v)
	}
	registration := ""
	if w.MusicRegistration != "" {
		registration, e = windowsPath(w.MusicRegistration)
		if e != nil {
			return e
		}
	}
	// The hidden broker must be bound to this exact host process, rather than
	// merely an integer PID (which Windows may later reuse).  If this process
	// crashes, the broker observes the handle signal, exits, and its LPAC job's
	// KILL_ON_JOB_CLOSE setting terminates the sandboxed child tree.
	parentPID, parentCreated, e := windowsParentIdentity()
	if e != nil {
		return fmt.Errorf("Windows父进程身份不可核验：%w", e)
	}
	requestDocument := map[string]any{"cwd": winDir, "command": command, "argv": argv, "shell": shell, "sandbox": true, "roots": mounts, "read_only": readonly, "music_policy": policy, "environment": environment, "music_registration": registration, "gpu_authorized": cfg.Isolation.GPU}
	// WSL can prepare a Windows request document but is not the native parent
	// process of the eventual pythonw.exe.  Omit the pair there for deployed
	// legacy generic helpers; an offline speech policy is rejected before this
	// path on non-Windows (RunSpeechIsolated).
	if parentCreated != 0 {
		requestDocument["parent_pid"] = parentPID
		requestDocument["parent_created_filetime"] = parentCreated
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline).Seconds()
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		requestDocument["timeout_seconds"] = remaining
	}
	b, _ := json.Marshal(requestDocument)
	if e = os.WriteFile(request, b, 0600); e != nil {
		return e
	}
	helperWin, e := windowsPath(w.Helper)
	if e != nil {
		return e
	}
	cmd := exec.CommandContext(context.Background(), w.Pythonw, helperWin, requestWin)
	configureProcess(cmd)
	if e = cmd.Start(); e != nil {
		return fmt.Errorf("Windows隐藏执行器启动失败，请核对受管pythonw.exe和本机执行环境：%w", e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	resultFile := filepath.Join(dir, "result.json")
	outputFile := filepath.Join(dir, "output.log")
	// Only the trusted music caller can request a bounded private copy of the
	// child's stderr. Generic Agent output never receives runtime diagnostics.
	readDiagnostic := func() {
		if diagnostic == nil {
			return
		}
		f, e := os.Open(filepath.Join(dir, "stderr.log"))
		if e != nil {
			return
		}
		defer f.Close()
		_, _ = io.CopyN(diagnostic, f, 64<<10)
	}
	var offset int64
	readOutput := func() {
		f, e := os.Open(outputFile)
		if e != nil {
			return
		}
		defer f.Close()
		if _, e = f.Seek(offset, 0); e != nil {
			return
		}
		n, _ := io.Copy(out, f)
		offset += n
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	var cancelAt time.Time
	var exitedAt time.Time
	var helperErr error
	for {
		readOutput()
		if b, e := os.ReadFile(resultFile); e == nil {
			// The child can flush its last bytes between the first read and
			// publication of result.json. Do not lose a structured response tail.
			readOutput()
			readDiagnostic()
			var result struct {
				ExitCode int    `json:"exit_code"`
				Error    string `json:"error"`
			}
			if json.Unmarshal(b, &result) != nil {
				return errors.New("Windows执行结果格式无效，请核对原进程，勿重放")
			}
			if result.Error != "" {
				_, _ = fmt.Fprintln(out, result.Error)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if result.ExitCode != 0 {
				return &windowsExitError{result.ExitCode}
			}
			return nil
		}
		if !cancelAt.IsZero() && time.Since(cancelAt) > 5*time.Second {
			_ = cmd.Process.Kill()
			return ctx.Err()
		}
		if !exitedAt.IsZero() && time.Since(exitedAt) > 2*time.Second {
			return errors.New("Windows隐藏执行器已退出但未产生结果；请核对暂存记录，勿重放")
		}
		select {
		case <-ctx.Done():
			if cancelAt.IsZero() {
				cancelAt = time.Now()
				_ = os.WriteFile(filepath.Join(dir, "cancel"), nil, 0600)
			}
			// Do not spin on a permanently ready cancellation channel.
			time.Sleep(100 * time.Millisecond)
		case helperErr = <-done:
			if helperErr != nil && ctx.Err() == nil {
				return errors.New("Windows隐藏执行器异常退出；请读取暂存记录，勿重放")
			}
			done = nil
			exitedAt = time.Now()
		case <-ticker.C:
		}
	}
}
