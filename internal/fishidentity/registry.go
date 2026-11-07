// Package fishidentity persists the local owner and compatible account links.
package fishidentity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/identity"
)

// Registry stores one locally owned fish and its account bindings separately
// from account credentials, activity, and the editable workspace.
type Registry struct {
	mu   sync.Mutex
	path string
}

type Record struct {
	Version        int              `json:"version"`
	FishID         string           `json:"fish_id,omitempty"`
	Bindings       []AccountBinding `json:"bindings"`
	DeviceBindings []DeviceBinding  `json:"device_bindings,omitempty"`
	LocalOwner     *LocalOwner      `json:"local_owner,omitempty"`
}

// LocalOwner is the stable local owner of this installation. It is
// intentionally independent of a connected application account: removing or
// logging out of WeChat must never erase the fish's own home or its grant.
type LocalOwner struct {
	ID         string `json:"id"`
	Revision   uint64 `json:"revision"`
	HomeAccess bool   `json:"home_access"`
}

// AccountBinding keeps the platform namespace in the durable key even
// though the first administration surface exposes WeChat accounts only.
// A same-looking account ID on a later platform can therefore never merge by
// accident with a WeChat account.
type AccountBinding struct {
	Platform string `json:"platform"`
	Account  string `json:"account_id"`
}

// DeviceBinding deliberately contains identifiers only. Device connection
// materials, screenshots and application credentials remain owned by their
// respective host registries; linking a phone must not copy them into the
// fish's identity record.
type DeviceBinding struct {
	Account   string `json:"account_id"`
	DeviceID  string `json:"device_id"`
	BindingID string `json:"binding_id"`
}

const (
	fishWechatPlatform          = "wechat"
	fishIdentityVersionV1       = 1
	fishIdentityVersionV2       = 2
	fishIdentityCurrentVersion  = 3
	fishDeviceBindingIDPrefix   = "fish-device-"
	fishDeviceBindingTokenChars = 48
	fishLocalOwnerIDPrefix      = "owner-"
	fishLocalOwnerTokenChars    = 48
)

var (
	ErrHomeRevisionConflict = errors.New("本机家目录授权已变更，请刷新后再试")
	ErrHomeNotInitialized   = errors.New("本机鱼身份尚未初始化")
	ErrRegistryBusy         = errors.New("鱼身份绑定正在被其他进程使用")
)

// New constructs a registry without writing an identity or grant. Operations
// validate the current record before use, including after a service restart.
func New(dir string) *Registry {
	return &Registry{path: filepath.Join(dir, "fish-identity.json")}
}

// Read returns the current record without creating an identity or grant.
func (r *Registry) Read() (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return Record{}, err
	}
	defer release()
	return r.readHeld()
}

// readHeld deliberately does not acquire a lock. All callers must already
// hold r.mu and the per-registry sidecar lock so a read-modify-write operation
// cannot lose a concurrent application's update.
func (r *Registry) readHeld() (Record, error) {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{Version: fishIdentityCurrentVersion, Bindings: []AccountBinding{}, DeviceBindings: []DeviceBinding{}}, nil
	}
	if err != nil || len(data) > 1<<20 {
		return Record{}, errors.New("鱼身份绑定不可读")
	}
	// Decode the version envelope before the record. json.Unmarshal ignores
	// unknown fields, which would make a v1 record carrying device grants look
	// harmless and could silently authorize a malformed migration.
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		return Record{}, errors.New("鱼身份绑定无效，原文件未覆盖")
	}
	versionRaw, present := envelope["version"]
	var version int
	if !present || json.Unmarshal(versionRaw, &version) != nil {
		return Record{}, errors.New("鱼身份绑定无效，原文件未覆盖")
	}
	if version == fishIdentityVersionV1 {
		if _, hasDeviceBindings := envelope["device_bindings"]; hasDeviceBindings {
			return Record{}, errors.New("鱼身份绑定无效，原文件未覆盖")
		}
		if _, hasLocalOwner := envelope["local_owner"]; hasLocalOwner {
			return Record{}, errors.New("鱼身份绑定无效，原文件未覆盖")
		}
	} else if version == fishIdentityVersionV2 {
		if _, hasLocalOwner := envelope["local_owner"]; hasLocalOwner {
			return Record{}, errors.New("鱼身份绑定无效，原文件未覆盖")
		}
	} else if version != fishIdentityCurrentVersion {
		return Record{}, errors.New("鱼身份绑定无效，原文件未覆盖")
	}
	var record Record
	if json.Unmarshal(data, &record) != nil || record.Version != version || !ValidFishID(record.FishID) {
		return Record{}, errors.New("鱼身份绑定无效，原文件未覆盖")
	}
	seen := map[string]bool{}
	for _, binding := range record.Bindings {
		key := binding.Platform + "\x00" + binding.Account
		if binding.Platform != fishWechatPlatform || !identity.ValidPathID(binding.Account) || seen[key] {
			return Record{}, errors.New("鱼身份绑定无效，原文件未覆盖")
		}
		seen[key] = true
	}
	if record.Bindings == nil {
		record.Bindings = []AccountBinding{}
	}
	if record.Version >= fishIdentityVersionV2 {
		seenDevices := map[string]bool{}
		seenBindingIDs := map[string]bool{}
		for _, binding := range record.DeviceBindings {
			key := binding.Account + "\x00" + binding.DeviceID
			if !fishAccountIsBound(record, binding.Account) || !identity.ValidLegacyDeviceID(binding.DeviceID) || !validFishDeviceBindingID(binding.BindingID) || seenDevices[key] || seenBindingIDs[binding.BindingID] {
				return Record{}, errors.New("鱼设备绑定无效，原文件未覆盖")
			}
			seenDevices[key] = true
			seenBindingIDs[binding.BindingID] = true
		}
	}
	if record.DeviceBindings == nil {
		record.DeviceBindings = []DeviceBinding{}
	}
	if record.Version == fishIdentityCurrentVersion && record.LocalOwner != nil && !ValidLocalOwner(*record.LocalOwner) {
		return Record{}, errors.New("本机鱼主人无效，原文件未覆盖")
	}
	return record, nil
}

func fishAccountIsBound(record Record, account string) bool {
	return slices.Contains(record.Bindings, AccountBinding{Platform: fishWechatPlatform, Account: account})
}

func validFishDeviceBindingID(id string) bool {
	if len(id) != len(fishDeviceBindingIDPrefix)+fishDeviceBindingTokenChars || !strings.HasPrefix(id, fishDeviceBindingIDPrefix) {
		return false
	}
	for _, r := range id[len(fishDeviceBindingIDPrefix):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func ValidFishID(id string) bool {
	if len(id) != len("fish-")+48 || !strings.HasPrefix(id, "fish-") {
		return false
	}
	for _, r := range id[len("fish-"):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func ValidLocalOwner(owner LocalOwner) bool {
	if owner.Revision == 0 || len(owner.ID) != len(fishLocalOwnerIDPrefix)+fishLocalOwnerTokenChars || !strings.HasPrefix(owner.ID, fishLocalOwnerIDPrefix) {
		return false
	}
	for _, r := range owner.ID[len(fishLocalOwnerIDPrefix):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func (r *Registry) acquire() (func(), error) {
	release, err := hostfile.Lock(r.path + ".lock")
	if err != nil {
		return nil, ErrRegistryBusy
	}
	return release, nil
}

func (r *Registry) Bindings() (string, map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return "", nil, err
	}
	defer release()
	record, err := r.readHeld()
	if err != nil {
		return "", nil, err
	}
	bound := make(map[string]bool, len(record.Bindings))
	for _, binding := range record.Bindings {
		if binding.Platform == fishWechatPlatform {
			bound[binding.Account] = true
		}
	}
	return record.FishID, bound, nil
}

func (r *Registry) Bind(account string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !identity.ValidPathID(account) {
		return "", errors.New("账号标识无效")
	}
	release, err := r.acquire()
	if err != nil {
		return "", err
	}
	defer release()
	record, err := r.readHeld()
	if err != nil {
		return "", err
	}
	if record.FishID == "" {
		record.FishID = "fish-" + identity.RandomToken()[:48]
	}
	binding := AccountBinding{Platform: fishWechatPlatform, Account: account}
	if slices.Contains(record.Bindings, binding) {
		return record.FishID, nil
	}
	record.Bindings = append(record.Bindings, binding)
	slices.SortFunc(record.Bindings, func(i, j AccountBinding) int {
		if i.Platform == j.Platform {
			return strings.Compare(i.Account, j.Account)
		}
		return strings.Compare(i.Platform, j.Platform)
	})
	if err = r.write(record); err != nil {
		return "", err
	}
	return record.FishID, nil
}

// unbind intentionally keeps FishID even after the final account is removed.
// A later explicit binding resumes the same local fish rather than inventing a
// second identity.
func (r *Registry) Unbind(account string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return err
	}
	defer release()
	record, err := r.readHeld()
	if err != nil {
		return err
	}
	before := len(record.Bindings)
	record.Bindings = slices.DeleteFunc(record.Bindings, func(bound AccountBinding) bool {
		return bound.Platform == fishWechatPlatform && bound.Account == account
	})
	if len(record.Bindings) == before {
		return nil
	}
	// A phone can only be used through a currently bound account. Removing that
	// account must revoke every such link without touching another account's
	// device bindings.
	record.DeviceBindings = slices.DeleteFunc(record.DeviceBindings, func(bound DeviceBinding) bool {
		return bound.Account == account
	})
	return r.write(record)
}

// bindDevice creates an explicit local association between a fish-bound WeChat
// account and a registered mobile device ID. It is metadata only: no device
// session, application account, screenshot or private material is copied.
func (r *Registry) BindDevice(account, deviceID string) (fishID, bindingID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return "", "", err
	}
	defer release()
	record, err := r.readHeld()
	if err != nil {
		return "", "", err
	}
	if !fishAccountIsBound(record, account) {
		return "", "", errors.New("账号未绑定到鱼身份")
	}
	if !identity.ValidLegacyDeviceID(deviceID) {
		return "", "", errors.New("设备标识无效")
	}
	for _, binding := range record.DeviceBindings {
		if binding.Account == account && binding.DeviceID == deviceID {
			return record.FishID, binding.BindingID, nil
		}
	}
	bindingID = fishDeviceBindingIDPrefix + identity.RandomToken()[:fishDeviceBindingTokenChars]
	record.DeviceBindings = append(record.DeviceBindings, DeviceBinding{Account: account, DeviceID: deviceID, BindingID: bindingID})
	slices.SortFunc(record.DeviceBindings, func(i, j DeviceBinding) int {
		if i.Account == j.Account {
			return strings.Compare(i.DeviceID, j.DeviceID)
		}
		return strings.Compare(i.Account, j.Account)
	})
	if err := r.write(record); err != nil {
		return "", "", err
	}
	return record.FishID, bindingID, nil
}

// deviceBindings returns bindings only for one already fish-bound account.
// The returned slice is a copy, so callers cannot modify registry state.
func (r *Registry) DeviceBindings(account string) (fishID string, bindings []DeviceBinding, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return "", nil, err
	}
	defer release()
	record, err := r.readHeld()
	if err != nil {
		return "", nil, err
	}
	if !fishAccountIsBound(record, account) {
		return "", nil, errors.New("账号未绑定到鱼身份")
	}
	for _, binding := range record.DeviceBindings {
		if binding.Account == account {
			bindings = append(bindings, binding)
		}
	}
	return record.FishID, bindings, nil
}

// unbindDevice is conditional so a stale management page cannot unbind a
// newly-created replacement. An unbind/rebind always mints a fresh binding ID.
func (r *Registry) UnbindDevice(account, deviceID, expectedBindingID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return err
	}
	defer release()
	record, err := r.readHeld()
	if err != nil {
		return err
	}
	if !fishAccountIsBound(record, account) {
		return errors.New("账号未绑定到鱼身份")
	}
	if !identity.ValidLegacyDeviceID(deviceID) || !validFishDeviceBindingID(expectedBindingID) {
		return errors.New("鱼设备绑定已变更，请刷新后再试")
	}
	index := slices.IndexFunc(record.DeviceBindings, func(binding DeviceBinding) bool {
		return binding.Account == account && binding.DeviceID == deviceID
	})
	if index < 0 || record.DeviceBindings[index].BindingID != expectedBindingID {
		return errors.New("鱼设备绑定已变更，请刷新后再试")
	}
	record.DeviceBindings = slices.Delete(record.DeviceBindings, index, index+1)
	return r.write(record)
}

func (r *Registry) write(record Record) error {
	// Any write is an intentional migration to v3. Reading an older record
	// never creates or authorizes a local owner by itself.
	record.Version = fishIdentityCurrentVersion
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New("鱼身份绑定无法编码")
	}
	if err = hostfile.WritePrivate(r.path, data); err != nil {
		return errors.New("鱼身份绑定保存失败")
	}
	return nil
}
