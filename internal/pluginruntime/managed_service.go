package pluginruntime

// Managed services are trusted host adapters, not plugin subprocess launch
// requests. Only host code may resolve a verified installation into a Spec;
// neither JSON-RPC arguments nor plugin configuration is a command descriptor.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ManagedServiceSpec struct {
	// OwnerPlugin is fixed by the trusted host adapter, never RPC arguments.
	OwnerPlugin string
	// Binding identifies the exact verified installation/configuration snapshot.
	Binding         string
	Executable      string
	Arguments       []string
	Directory       string
	Environment     []string
	LogPath         string
	PendingJobsPath string
	StopInput       string
	StopTimeout     time.Duration
	// Check must revalidate current host registration. It is checked even when
	// a process already exists, so changed configuration cannot adopt it.
	Check func(context.Context) error
}

// ManagedServiceAdmission serializes command.Start with the host's drain/update
// barrier. It must invoke start exactly once, synchronously, only if admitted.
type ManagedServiceAdmission func(context.Context, func() error) error

type ManagedServiceStatus struct {
	Running  bool `json:"running"`
	Stopping bool `json:"stopping"`
	Busy     bool `json:"busy"`
}

type managedService struct {
	spec     ManagedServiceSpec
	command  *exec.Cmd
	input    io.WriteCloser
	done     chan struct{}
	stopOnce sync.Once
	stopping bool
	// holds counts host-side requests which are currently using this exact
	// supervisor. It is deliberately independent from the durable job journal:
	// an HTTP call can be in flight before its job state has been persisted.
	holds int
	// lastActivity is only changed while the registry lock is held.  It is a
	// lease for a host request, not evidence that an unowned listener exists.
	lastActivity time.Time
}

// ManagedServices owns only processes started by this registry. There is no
// PID import, process-name lookup, listener adoption, or arbitrary kill API.
// The zero value is ready to use.
type ManagedServices struct {
	mu         sync.Mutex
	processes  map[string]*managedService
	ownerStops map[string]int
	closed     bool
}

func (r *ManagedServices) Start(ctx context.Context, id string, spec ManagedServiceSpec, admit ManagedServiceAdmission) (ManagedServiceStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ManagedServiceStatus{}, errors.New("managed_service_registry_closed")
	}
	if err := ctx.Err(); err != nil {
		return ManagedServiceStatus{}, err
	}
	if id == "" || spec.OwnerPlugin == "" || spec.Binding == "" || spec.Check == nil || admit == nil || !filepath.IsAbs(spec.Executable) || !filepath.IsAbs(spec.Directory) || !filepath.IsAbs(spec.LogPath) || spec.StopTimeout <= 0 || len(spec.Environment) == 0 {
		return ManagedServiceStatus{}, errors.New("managed_service_descriptor_invalid")
	}
	if r.ownerStops[spec.OwnerPlugin] > 0 {
		return ManagedServiceStatus{}, errors.New("managed_service_owner_stopping")
	}
	if err := spec.Check(ctx); err != nil {
		return ManagedServiceStatus{}, err
	}
	if p := r.processes[id]; p != nil {
		select {
		case <-p.done:
			delete(r.processes, id)
		default:
			if p.spec.Binding != spec.Binding || p.spec.OwnerPlugin != spec.OwnerPlugin {
				return ManagedServiceStatus{}, errors.New("managed_service_registration_changed_stop_required")
			}
			if p.stopping {
				return statusManagedService(p), errors.New("managed_service_stopping")
			}
			// A repeated authorized start is the normal adjacent-request reuse
			// path.  Renew its lease without launching a second GPU service.
			p.lastActivity = time.Now()
			return statusManagedService(p), nil
		}
	}
	command := exec.Command(spec.Executable, append([]string(nil), spec.Arguments...)...)
	command.Dir, command.Env = spec.Directory, append([]string(nil), spec.Environment...)
	configureManagedService(command)
	input, err := command.StdinPipe()
	if err != nil {
		return ManagedServiceStatus{}, err
	}
	log, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		input.Close()
		return ManagedServiceStatus{}, err
	}
	command.Stdout, command.Stderr = log, log
	if err = spec.Check(ctx); err == nil {
		invoked := false
		err = admit(ctx, func() error {
			if invoked {
				return errors.New("managed_service_duplicate_admission")
			}
			invoked = true
			return command.Start()
		})
		if err == nil && command.Process == nil {
			err = errors.New("managed_service_not_started")
		}
	}
	if command.Process == nil {
		input.Close()
		log.Close()
		return ManagedServiceStatus{}, err
	}
	p := &managedService{spec: spec, command: command, input: input, done: make(chan struct{}), lastActivity: time.Now()}
	if r.processes == nil {
		r.processes = make(map[string]*managedService)
	}
	r.processes[id] = p
	go func() {
		_ = command.Wait()
		input.Close()
		log.Close()
		close(p.done)
	}()
	if err != nil {
		// A host admission callback must not lose ownership if it reports an
		// error after starting. Retain/reap and stop that exact owned process.
		p.stopping = true
		_ = command.Process.Kill()
		return statusManagedService(p), err
	}
	return statusManagedService(p), nil
}

func statusManagedService(p *managedService) ManagedServiceStatus {
	if p == nil {
		return ManagedServiceStatus{}
	}
	select {
	case <-p.done:
		return ManagedServiceStatus{}
	default:
	}
	return ManagedServiceStatus{Running: true, Stopping: p.stopping, Busy: p.holds > 0 || pendingManagedServiceJobs(p.spec.PendingJobsPath)}
}

// Touch extends the idle lease of a process already owned by this registry.
// It deliberately cannot create, discover or adopt a process.  A false result
// means callers must use their normal, authorized start/recovery path.
func (r *ManagedServices) Touch(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.processes[id]
	if p == nil || p.stopping {
		return false
	}
	select {
	case <-p.done:
		delete(r.processes, id)
		return false
	default:
		p.lastActivity = time.Now()
		return true
	}
}

// Acquire holds an already-owned service across one host-side operation. The
// release closure is bound to the exact process instance, so a delayed caller
// cannot refresh or release a later process that reused the same registry ID.
// It grants neither launch authority nor any handle to the child process.
func (r *ManagedServices) Acquire(id string) (release func(), ok bool) {
	r.mu.Lock()
	p := r.processes[id]
	if p == nil || p.stopping {
		r.mu.Unlock()
		return nil, false
	}
	select {
	case <-p.done:
		delete(r.processes, id)
		r.mu.Unlock()
		return nil, false
	default:
	}
	p.holds++
	p.lastActivity = time.Now()
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.processes[id] != p || p.holds == 0 {
				return
			}
			p.holds--
			p.lastActivity = time.Now()
		})
	}, true
}

// IdleFor reports the owned service's quiet period.  It is intentionally
// unavailable for a stopping or exited service so an idle reaper cannot race
// a shutdown or turn a stale pointer into a new process target.
func (r *ManagedServices) IdleFor(id string, now time.Time) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.processes[id]
	if p == nil || p.stopping {
		return 0, false
	}
	select {
	case <-p.done:
		delete(r.processes, id)
		return 0, false
	default:
	}
	if p.lastActivity.IsZero() || now.Before(p.lastActivity) {
		return 0, false
	}
	return now.Sub(p.lastActivity), true
}

// StopIfIdle reclaims exactly one registry-owned process after an idle lease.
// Pending work is checked while reserving the stop.  Unknown, malformed and
// nonterminal journals are busy (see pendingManagedServiceJobs), so this must
// never stop a conversion whose provider receipt is still uncertain.
func (r *ManagedServices) StopIfIdle(ctx context.Context, id string, timeout time.Duration) (bool, error) {
	return r.stopIfIdle(ctx, id, timeout, nil)
}

// StopIfIdleGuarded makes the final stop reservation only if guard still
// authorizes it. The guard runs while the service registry is locked, just
// before p.stopping is set; callers use it to invalidate a timer whose policy
// changed after it fired. It must not call back into this registry.
func (r *ManagedServices) StopIfIdleGuarded(ctx context.Context, id string, timeout time.Duration, guard func() bool) (bool, error) {
	if guard == nil {
		return false, errors.New("managed_service_idle_guard_invalid")
	}
	return r.stopIfIdle(ctx, id, timeout, guard)
}

func (r *ManagedServices) stopIfIdle(ctx context.Context, id string, timeout time.Duration, guard func() bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if timeout <= 0 {
		return false, nil
	}
	r.mu.Lock()
	p := r.processes[id]
	if p == nil || p.stopping {
		r.mu.Unlock()
		return false, nil
	}
	select {
	case <-p.done:
		delete(r.processes, id)
		r.mu.Unlock()
		return false, nil
	default:
	}
	if p.holds > 0 || pendingManagedServiceJobs(p.spec.PendingJobsPath) || p.lastActivity.IsZero() || time.Since(p.lastActivity) < timeout {
		r.mu.Unlock()
		return false, nil
	}
	if guard != nil && !guard() {
		r.mu.Unlock()
		return false, nil
	}
	// Reserve before releasing the registry lock.  A later Touch then returns
	// false rather than racing a stop with a new request.
	p.stopping = true
	r.mu.Unlock()
	if err := stopManagedService(ctx, p); err != nil {
		return false, err
	}
	return true, nil
}

// A missing journal means no submitted jobs. An unreadable, oversized or
// malformed journal cannot prove idleness and conservatively blocks draining.
func pendingManagedServiceJobs(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil || len(data) > 32<<20 {
		return true
	}
	var jobs map[string]*struct {
		State string `json:"state"`
	}
	if json.Unmarshal(data, &jobs) != nil || jobs == nil {
		return true
	}
	for _, job := range jobs {
		if job == nil || job.State == "" {
			return true
		}
		// The local journal is also the recovery source after a lost network
		// reply.  Only explicit terminal states prove this supervisor is safe to
		// reclaim; accepted/submitted/unknown must continue to occupy it until
		// their durable delivery/recovery path settles.
		switch strings.ToLower(strings.TrimSpace(job.State)) {
		case "completed", "failed", "cancelled", "canceled", "stopped", "delivered":
		default:
			return true
		}
	}
	return false
}

func (r *ManagedServices) Status(id string) ManagedServiceStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return statusManagedService(r.processes[id])
}

// ProcessID returns only the PID of a still-running process this registry
// started itself. It deliberately cannot import, discover or control an
// external PID. Host adapters use this narrow proof to reject a listener that
// raced their dedicated loopback port after admission.
func (r *ManagedServices) ProcessID(id string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.processes[id]
	if p == nil || p.stopping || p.command == nil || p.command.Process == nil {
		return 0, false
	}
	select {
	case <-p.done:
		delete(r.processes, id)
		return 0, false
	default:
	}
	if p.command.Process.Pid <= 0 {
		return 0, false
	}
	return p.command.Process.Pid, true
}

// Busy lets account removal/update barriers cover every registered adapter,
// without adding a new typed Manager field for each service.
func (r *ManagedServices) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, process := range r.processes {
		if statusManagedService(process).Busy {
			return true
		}
	}
	return false
}

func (r *ManagedServices) OwnerBusy(owner string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, process := range r.processes {
		if process.spec.OwnerPlugin == owner && statusManagedService(process).Busy {
			return true
		}
	}
	return false
}

// StopOwner refuses a busy owner before stopping any of its services. It
// reserves that owner against new launches while waiting, and captures owned
// process pointers so an exited/reused registry ID cannot target another owner.
func (r *ManagedServices) StopOwner(ctx context.Context, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if owner == "" {
		return errors.New("managed_service_owner_invalid")
	}
	r.mu.Lock()
	processes := make([]*managedService, 0)
	for _, process := range r.processes {
		if process.spec.OwnerPlugin != owner {
			continue
		}
		if statusManagedService(process).Busy {
			r.mu.Unlock()
			return errors.New("managed_service_owner_busy")
		}
		processes = append(processes, process)
	}
	if r.ownerStops == nil {
		r.ownerStops = make(map[string]int)
	}
	r.ownerStops[owner]++
	for _, process := range processes {
		process.stopping = true
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.ownerStops[owner]--
		if r.ownerStops[owner] == 0 {
			delete(r.ownerStops, owner)
		}
		r.mu.Unlock()
	}()
	return stopManagedServices(ctx, processes)
}

func (r *ManagedServices) Stop(ctx context.Context, id string) error {
	r.mu.Lock()
	p := r.processes[id]
	if p == nil {
		r.mu.Unlock()
		return nil
	}
	p.stopping = true
	r.mu.Unlock()
	return stopManagedService(ctx, p)
}

func stopManagedService(ctx context.Context, p *managedService) error {
	p.stopOnce.Do(func() {
		go func() {
			// Closing stdin also tells a supervisor to stop if its protocol input
			// cannot be written. The timeout kills only this owned supervisor.
			if p.spec.StopInput != "" {
				_, _ = io.WriteString(p.input, p.spec.StopInput)
			}
			p.input.Close()
		}()
	})
	timer := time.NewTimer(p.spec.StopTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		_ = p.command.Process.Kill()
	}
	// Wait is bounded even if the operating system fails to reap a process.
	timer.Reset(5 * time.Second)
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("managed_service_stop_unconfirmed")
	}
}

func (r *ManagedServices) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	processes := make([]*managedService, 0, len(r.processes))
	for _, process := range r.processes {
		process.stopping = true
		processes = append(processes, process)
	}
	r.mu.Unlock()
	return stopManagedServices(ctx, processes)
}

func stopManagedServices(ctx context.Context, processes []*managedService) error {
	var joined error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, process := range processes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := stopManagedService(ctx, process)
			mu.Lock()
			joined = errors.Join(joined, err)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return joined
}
