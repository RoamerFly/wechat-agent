package pluginruntime

import (
	"context"
	"errors"

	"wechat-bot/internal/pluginhost"
)

// ConfigureIdle changes only configuration. Enabled intent, grants, package
// version and durable jobs remain unchanged. An idle session is discarded before
// the commit so EnsureStarted must load the new configuration; a failed update
// retains the old configuration and may leave its runtime awaiting on-demand
// restart. The reservation is shared with Invoke and all lifecycle operations.
func (c *Controller) ConfigureIdle(ctx context.Context, id string, input pluginhost.ConfigUpdate) (config pluginhost.Configuration, err error) {
	if err = ctx.Err(); err != nil {
		return config, err
	}
	if err = c.begin(id); err != nil {
		return config, err
	}
	defer func() { c.end(id, err) }()
	if _, err = c.store.Installed(ctx, id); err != nil {
		return config, err
	}
	current, err := c.store.Configuration(ctx, id)
	if err != nil {
		return config, err
	}
	if current.Revision != input.Revision {
		return config, pluginhost.ErrRevision
	}
	pending, err := c.store.HasUnfinishedJobs(ctx, id)
	if err != nil {
		return config, err
	}
	if pending || c.host.Status(id).ActiveCalls != 0 {
		return config, errors.New("plugin configuration has unfinished work")
	}
	c.mu.Lock()
	services := c.services
	c.mu.Unlock()
	if services != nil && services.OwnerBusy(id) {
		return config, errors.New("plugin service is busy")
	}
	if c.host.Status(id).Registered {
		if err = c.host.Disable(id); err != nil {
			return config, err
		}
	}
	c.mu.Lock()
	delete(c.loaded, id)
	delete(c.automatic, id)
	c.notifyIdleLocked()
	c.mu.Unlock()
	return c.store.SaveIdleConfiguration(ctx, id, input)
}
