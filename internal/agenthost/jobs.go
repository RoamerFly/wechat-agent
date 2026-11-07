package agenthost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Process struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	Key       string    `json:"request_key"`
	Hash      string    `json:"request_hash"`
	State     string    `json:"state"`
	Output    string    `json:"output"`
	ExitCode  int       `json:"exit_code"`
	Truncated bool      `json:"truncated"`
	Started   time.Time `json:"started"`
	Updated   time.Time `json:"updated"`
	Reason    string    `json:"reason,omitempty"`
	cancel    context.CancelFunc
	buffer    *limitedOutput
}
type Jobs struct {
	mu     sync.Mutex
	root   string
	items  map[string]*Process
	closed bool
	wg     sync.WaitGroup
}

func (j *Jobs) Busy() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, p := range j.items {
		if p.State == "running" {
			return true
		}
	}
	return false
}

func OpenJobs(root string) (*Jobs, error) {
	if e := os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	j := &Jobs{root: root, items: map[string]*Process{}}
	files, e := os.ReadDir(root)
	if e != nil {
		return nil, e
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(root, f.Name()))
		var p Process
		if e != nil || len(b) > 512<<10 || json.Unmarshal(b, &p) != nil || p.ID+".json" != f.Name() {
			return nil, errors.New("进程记录不可核验，未覆盖")
		}
		if p.State == "running" {
			p.State = "interrupted"
			p.Reason = "management_restarted_no_replay"
			p.Updated = time.Now().UTC()
			if e = j.save(&p); e != nil {
				return nil, e
			}
		}
		j.items[p.ID] = &p
	}
	return j, nil
}
func (j *Jobs) save(p *Process) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(j.root, ".process-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	e = f.Chmod(0600)
	if e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), filepath.Join(j.root, p.ID+".json"))
}
func processCopy(p *Process) Process {
	out := *p
	out.buffer = nil
	out.cancel = nil
	if p.buffer != nil {
		p.buffer.mu.Lock()
		out.Output = strings.ToValidUTF8(string(p.buffer.data), "�")
		out.Truncated = p.buffer.truncated
		p.buffer.mu.Unlock()
	}
	return out
}
func (j *Jobs) List(scope string) []Process {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := []Process{}
	for _, p := range j.items {
		if scope == "" || p.Scope == scope {
			out = append(out, processCopy(p))
		}
	}
	return out
}

// FindByKey is a scoped read of the authoritative job, including a job whose
// start response was lost. It never submits or adopts work.
func (j *Jobs) FindByKey(scope, key string) (Process, bool) {
	if scope == "" || key == "" {
		return Process{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, p := range j.items {
		if p.Scope == scope && p.Key == key {
			return processCopy(p), true
		}
	}
	return Process{}, false
}

func (j *Jobs) Status(scope, id string, cancel bool) (Process, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	p := j.items[id]
	if p == nil || scope != "" && p.Scope != scope {
		return Process{}, errors.New("当前会话找不到此后台进程")
	}
	if cancel && p.cancel != nil {
		p.cancel()
	}
	return processCopy(p), nil
}
func (j *Jobs) Start(cfg Config, scope, key, path, command string, seconds int, allowed func() bool) (Process, error) {
	return j.start(cfg, scope, key, path, command, nil, seconds, allowed, false)
}
func (j *Jobs) StartWindows(cfg Config, scope, key, path, command string, seconds int, allowed func() bool) (Process, error) {
	return j.start(cfg, scope, key, path, command, nil, seconds, allowed, true)
}
func (j *Jobs) StartWindowsArgv(cfg Config, scope, key, path string, argv []string, seconds int, allowed func() bool) (Process, error) {
	return j.start(cfg, scope, key, path, "", argv, seconds, allowed, true)
}

// executableUnderGrantedRoot is intentionally narrower than a shell command:
// direct Windows argv mode may start only a real .exe below an already granted
// workspace/read-only root. Every path component must be a normal directory;
// a junction/symlink cannot turn a harmless public runtime entry into a host
// control-plane mount.
func executableUnderGrantedRoot(cfg Config, executable string) error {
	if !strings.EqualFold(filepath.Ext(executable), ".exe") {
		return errors.New("Windows直接执行只允许已授权的exe文件")
	}
	for _, base := range append(append([]string{}, cfg.Roots...), cfg.Isolation.ReadOnly...) {
		cleanBase, cleanExecutable := filepath.Clean(base), filepath.Clean(executable)
		rel, err := filepath.Rel(cleanBase, cleanExecutable)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		part := cleanBase
		valid := true
		for _, name := range append([]string{"."}, strings.Split(rel, string(filepath.Separator))...) {
			if name != "." {
				part = filepath.Join(part, name)
			}
			info, statErr := os.Lstat(part)
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
				valid = false
				break
			}
		}
		if !valid {
			return errors.New("Windows直接执行路径包含链接或不可用组件")
		}
		info, statErr := os.Stat(cleanExecutable)
		if statErr == nil && info.Mode().IsRegular() {
			return nil
		}
		return errors.New("Windows直接执行文件不可用")
	}
	return errors.New("Windows直接执行文件不在已授权目录内")
}

func (j *Jobs) start(cfg Config, scope, key, path, command string, argv []string, seconds int, allowed func() bool, windows bool) (Process, error) {
	if cfg.Isolation == nil {
		return Process{}, errors.New("sandbox_required: legacy unrestricted execution is disabled")
	}
	if windows {
		if cfg.Windows == nil {
			return Process{}, errors.New("Windows隐藏执行桥接未配置；请配置pythonw、helper和spool路径")
		}
		var e error
		path, e = WindowsLocalPath(path)
		if e != nil {
			return Process{}, e
		}
	}
	if cfg.Mode != "full" || cfg.Validate() != nil || scope == "" || len(key) < 1 || len(key) > 128 || len(command) > 8000 || strings.TrimSpace(command) == "" && len(argv) == 0 || strings.TrimSpace(command) != "" && len(argv) != 0 {
		return Process{}, errors.New("命令执行未授权或参数无效")
	}
	if len(argv) > 0 {
		if !windows || !filepath.IsAbs(argv[0]) {
			return Process{}, errors.New("Windows直接执行参数无效")
		}
		for _, value := range argv {
			if strings.ContainsRune(value, 0) || len(value) > 8192 {
				return Process{}, errors.New("Windows直接执行参数无效")
			}
		}
		if err := executableUnderGrantedRoot(cfg, argv[0]); err != nil {
			return Process{}, err
		}
	}
	if seconds < 0 || int64(seconds) > int64((1<<63-1)/time.Second) {
		return Process{}, errors.New("后台执行时限无效；0表示不设时限")
	}
	root, rel, e := (Host{cfg}).rooted(path)
	if e != nil {
		return Process{}, e
	}
	info, e := root.Stat(rel)
	root.Close()
	if e != nil || !info.IsDir() {
		return Process{}, errors.New("执行起始目录不可用")
	}
	// Keep the durable identity byte-for-byte compatible for command mode.
	// Direct argv is a distinct, explicitly tagged protocol shape so it can
	// never collide with an old shell command request key.
	identity := []any{path, command, seconds}
	if len(argv) > 0 {
		identity = []any{"windows-direct-argv-v1", path, argv, seconds}
	}
	if windows {
		identity = append(identity, "windows")
	}
	b, _ := json.Marshal(identity)
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return Process{}, errors.New("进程管理器已关闭")
	}
	for _, p := range j.items {
		if p.Scope == scope && p.Key == key {
			if p.Hash != hash {
				return Process{}, errors.New("同一请求编号对应了不同命令；未重复执行")
			}
			return processCopy(p), nil
		}
	}
	if allowed != nil && !allowed() {
		return Process{}, errors.New("执行权限已撤销")
	}
	id := make([]byte, 16)
	if _, e = rand.Read(id); e != nil {
		return Process{}, e
	}
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	if seconds > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	}
	p := &Process{ID: "process-" + hex.EncodeToString(id), Scope: scope, Key: key, Hash: hash, State: "running", Started: now, Updated: now, cancel: cancel, buffer: &limitedOutput{limit: cfg.OutputBytes, keepTail: true}}
	if e = j.save(p); e != nil {
		cancel()
		return Process{}, e
	}
	j.items[p.ID] = p
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		defer cancel()
		done := make(chan struct{})
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					if allowed != nil && !allowed() {
						cancel()
						return
					}
					j.mu.Lock()
					snapshot := processCopy(p)
					_ = j.save(&snapshot)
					j.mu.Unlock()
				}
			}
		}()
		var err error
		if windows {
			if len(argv) > 0 {
				err = runWindowsRequest(ctx, cfg, p.ID, path, "", argv, p.buffer)
			} else {
				err = runWindowsHidden(ctx, cfg, p.ID, path, command, p.buffer)
			}
		} else {
			cmd, buildErr := isolatedCommand(ctx, cfg, path, command)
			if buildErr != nil {
				err = buildErr
			} else {
				cmd.Stdout, cmd.Stderr = p.buffer, p.buffer
				err = cmd.Run()
			}
		}
		if err != nil {
			_, _ = fmt.Fprintln(p.buffer, "执行未完成："+err.Error())
		}
		close(done)
		j.mu.Lock()
		defer j.mu.Unlock()
		snapshot := processCopy(p)
		p.Output = snapshot.Output
		p.Truncated = snapshot.Truncated
		p.buffer = nil
		p.cancel = nil
		p.Updated = time.Now().UTC()
		p.State = "completed"
		p.ExitCode = 0
		if err != nil {
			p.State = "failed"
			p.ExitCode = -1
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				p.ExitCode = exit.ExitCode()
			}
			var windowsExit *windowsExitError
			if errors.As(err, &windowsExit) {
				p.ExitCode = windowsExit.code
			}
		}
		if ctx.Err() != nil {
			p.State = "cancelled"
			p.Reason = "cancelled_or_permission_revoked"
			if ctx.Err() == context.DeadlineExceeded {
				p.State = "timed_out"
				p.Reason = "process_deadline"
			}
		}
		if err = j.save(p); err != nil {
			p.State = "needs_review"
			p.Reason = "process_result_persistence_failed"
		}
	}()
	return processCopy(p), nil
}

// BeginShutdown prevents any later Start from registering a process and
// cancels every process already registered.  The same mutex also protects the
// final Start check, so a desktop stop and a late tool call have a defined
// order rather than a check-then-register gap.
func (j *Jobs) BeginShutdown() {
	j.mu.Lock()
	j.closed = true
	for _, p := range j.items {
		if p.cancel != nil {
			p.cancel()
		}
	}
	j.mu.Unlock()
}

// Close waits for running process bookkeeping after BeginShutdown. Callers
// that need a bounded desktop exit can invoke BeginShutdown first and put this
// wait behind their own context deadline.
func (j *Jobs) Close() {
	j.BeginShutdown()
	j.wg.Wait()
}
