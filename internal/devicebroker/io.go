package devicebroker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"wechat-bot/internal/adbserver"
	"wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
)

// IORunner is a host-only execution seam. Args are always created by the fixed
// IO switch from the original authorized request, never accepted from a plugin.
type IORunner func(context.Context, string, []string, int64, time.Duration) ([]byte, error)

// A failed process-tree cleanup retains its OS owner until Close confirms it.
// This never carries an operation to retry.
type ioCleanupError struct {
	err     error
	cleanup func() error
}

func (e *ioCleanupError) Error() string { return "device_io_cleanup_unconfirmed" }
func (e *ioCleanupError) Unwrap() error { return e.err }
func (e *ioCleanupError) Close() error  { return e.cleanup() }

// InspectIOOptions reads the same profile/path/ADB allowlists as OpenIO. It
// creates no directories, starts no child and never acquires the ADB server.
func InspectIOOptions(ctx context.Context, o Options) error {
	o.inspectOnly = true
	if _, err := pluginhost.Verify(o.PackageDirectory); err != nil {
		return ErrNeedsMigration
	}
	_, err := resolve(ctx, o)
	return err
}

// InspectIOGrant reads the already registered profile and returns only its
// public app/action allowlist.  Like InspectIOOptions it does not create
// directories, launch a child process, acquire ADB, or contact a device.
func InspectIOGrant(ctx context.Context, o Options) ([]devicespec.IOGrant, error) {
	o.inspectOnly = true
	if _, err := pluginhost.Verify(o.PackageDirectory); err != nil {
		return nil, ErrNeedsMigration
	}
	r, err := resolve(ctx, o)
	if err != nil {
		return nil, err
	}
	grants := make([]devicespec.IOGrant, 0, len(r.profile.Devices))
	for id, device := range r.profile.Devices {
		grants = append(grants, devicespec.IOGrant{
			DeviceID: id,
			Packages: append([]string(nil), device.Packages...),
			Actions:  append([]string(nil), device.Actions...),
		})
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].DeviceID < grants[j].DeviceID })
	return grants, nil
}

type ioBroker struct {
	mu                    sync.Mutex
	cleanupMu             sync.Mutex
	proof                 *resourceProof
	profile               profile
	storage, adb, binding string
	leaseDirectory        string
	leases                map[string]string
	adbLease              adbserver.Lease
	run                   IORunner
	acquire               func(context.Context, string) (adbserver.Lease, error)
	db                    *sql.DB
	active                *ioCall
	closed                bool
	resourcesClosed       bool
}

func OpenIO(ctx context.Context, o Options) (devicespec.IOBroker, error) {
	if err := InspectIOOptions(ctx, o); err != nil {
		return nil, err
	}
	r, err := resolve(ctx, o)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (devicespec.IOBroker, error) { _ = r.adbLease.Close(); return nil, err }
	blobs, err := ensurePrivateDirectory(filepath.Join(r.storage, "io-blobs"))
	if err != nil || !within(r.storage, blobs) {
		return fail(ErrNeedsMigration)
	}
	r.dirs = append(r.dirs, blobs)
	r.privateDirs = append(r.privateDirs, blobs)
	manifest, err := makeSeal(filepath.Join(o.PackageDirectory, "plugin.json"))
	if err != nil {
		return fail(err)
	}
	r.seals = append(r.seals, manifest)
	proof := &resourceProof{seals: r.seals, dirs: r.dirs, privateDirs: r.privateDirs}
	db, err := sql.Open("sqlite", filepath.Join(r.storage, "device-ledger.sqlite"))
	if err != nil {
		return fail(err)
	}
	if _, err = db.ExecContext(ctx, `PRAGMA synchronous=FULL; CREATE TABLE IF NOT EXISTS operation(key TEXT PRIMARY KEY,hash TEXT NOT NULL,device TEXT NOT NULL,state TEXT NOT NULL,result TEXT NOT NULL,at REAL NOT NULL)`); err != nil {
		_ = db.Close()
		return fail(err)
	}
	run := o.Runner
	if run == nil {
		run = runFixedADB
	}
	manager := o.ADBServer
	if manager == nil {
		manager = defaultADBServer{}
	}
	return &ioBroker{proof: proof, profile: r.profile, storage: r.storage, adb: r.adb, binding: brokerBinding(r.seals, r.leases, r.leaseDirectory, r.adbLease.Binding()), leaseDirectory: r.leaseDirectory, leases: r.leases, adbLease: r.adbLease, run: run, db: db, acquire: manager.Acquire}, nil
}

func (b *ioBroker) Binding() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || !b.proof.verified() {
		return "", errChanged
	}
	return b.binding, nil
}
func (b *ioBroker) Begin(ctx context.Context, name string, raw json.RawMessage) (devicespec.IOCall, error) {
	if err := devicespec.CheckIOAuthority(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.active != nil || !b.proof.verified() {
		return nil, errChanged
	}
	if !allowedTool(name) {
		return nil, errToolDenied
	}
	var request map[string]any
	if json.Unmarshal(raw, &request) != nil || request == nil {
		return nil, errToolDenied
	}
	if err := validateIORequest(name, request, b.profile); err != nil {
		return nil, err
	}
	device, _ := request["device_id"].(string)
	var lease *physicalLease
	if name != "devices" {
		key, ok := b.leases[device]
		if !ok {
			return nil, errToolDenied
		}
		var err error
		lease, err = acquireLease(filepath.Join(b.leaseDirectory, "device-"+key+".lock"))
		if err != nil {
			if errors.Is(err, errLeaseBusy) {
				return nil, ErrDeviceBusy
			}
			return nil, err
		}
	}
	bounded, cancel := context.WithCancel(ctx)
	c := &ioCall{broker: b, ctx: bounded, cancel: cancel, name: name, request: request, device: device, lease: lease, started: time.Now(), refs: map[string]blobRef{}}
	b.active = c
	return c, nil
}
func (b *ioBroker) Close() error {
	b.cleanupMu.Lock()
	defer b.cleanupMu.Unlock()
	b.mu.Lock()
	if b.resourcesClosed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	c := b.active
	b.mu.Unlock()
	if c != nil {
		c.cancel()
		if err := c.Close(); err != nil {
			registerQuarantine(b)
			return err
		}
	}
	if err := errors.Join(b.db.Close(), b.adbLease.Close()); err != nil {
		registerQuarantine(b)
		return err
	}
	b.mu.Lock()
	b.resourcesClosed = true
	b.mu.Unlock()
	unregisterQuarantine(b)
	return nil
}

type ioCall struct {
	mu                          sync.Mutex
	broker                      *ioBroker
	ctx                         context.Context
	cancel                      context.CancelFunc
	name, device                string
	request                     map[string]any
	lease                       *physicalLease
	started                     time.Time
	closed, attempted, executed bool
	refs                        map[string]blobRef
	shot                        *blobRef
	foreground                  string
	ui                          string
	states                      map[string]string
	prior                       json.RawMessage
	artifact                    json.RawMessage
	chunk                       json.RawMessage
	cleanups                    []*ioCleanupError
}

func (c *ioCall) check(ctx context.Context) error {
	if err := devicespec.CheckIOAuthority(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ctx.Err(); err != nil {
		return err
	}
	c.broker.mu.Lock()
	brokerClosed := c.broker.closed
	c.broker.mu.Unlock()
	if c.closed || brokerClosed || !c.broker.proof.verified() {
		return errChanged
	}
	return nil
}
func (c *ioCall) Close() error {
	c.cancel()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !c.mu.TryLock() {
		select {
		case <-deadline.C:
			c.broker.mu.Lock()
			c.broker.closed = true
			c.broker.mu.Unlock()
			registerQuarantine(c.broker)
			return errors.New("device_io_cleanup_unconfirmed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	for _, pending := range c.cleanups {
		if err := pending.Close(); err != nil {
			c.broker.mu.Lock()
			c.broker.closed = true
			c.broker.mu.Unlock()
			registerQuarantine(c.broker)
			return err
		}
	}
	c.cleanups = nil
	if c.lease != nil {
		if err := c.lease.Close(); err != nil {
			registerQuarantine(c.broker)
			return err
		}
	}
	c.closed = true
	c.broker.mu.Lock()
	if c.broker.active == c {
		c.broker.active = nil
	}
	c.broker.mu.Unlock()
	return nil
}

func privateIOWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".device-io-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
