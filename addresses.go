package caddybgp

import (
	"errors"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/m-vinc/caddy-bgp/internal/anycast"
)

func (r *runtime) applyAddresses(planned *plan) error {
	if r.manager != nil && r.manager.Name() != planned.interfaceName {
		r.retireManager()
	}

	if planned.interfaceName == "" {
		return nil
	}

	if r.manager == nil {
		r.manager = r.reviveManager(planned.interfaceName)
	}

	added, err := r.manager.Add(planned.addresses)
	for _, address := range added {
		r.logger.Info("added address", zap.String("interface", r.manager.Name()), zap.String("address", address.String()))
	}

	return err
}

func (r *runtime) reviveManager(name string) *anycast.Manager {
	for index, manager := range r.retired {
		if manager.Name() == name {
			r.retired = slices.Delete(r.retired, index, index+1)
			return manager
		}
	}

	return anycast.NewManager(r.kernel, name)
}

func (r *runtime) retireManager() {
	if r.manager != nil {
		r.retired = append(r.retired, r.manager)
		r.manager = nil
	}
}

func (r *runtime) schedulePrune(planned *plan) {
	if r.prune != nil {
		r.prune.Stop()
		r.prune = nil
	}

	if r.manager == nil && len(r.retired) == 0 {
		return
	}

	r.prune = time.AfterFunc(planned.drainDelay, r.pruneAddresses)
}

func (r *runtime) pruneAddresses() {
	r.mu.Lock()
	defer r.mu.Unlock()

	owner := r.owner()
	if owner == nil {
		return
	}

	if r.manager != nil {
		removed, err := r.manager.Prune(owner.plan.addresses)
		r.logRemoved(r.manager, len(removed), err)
	}

	_ = r.closeRetired()
}

func (r *runtime) closeRetired() error {
	var failures []error

	for _, manager := range r.retired {
		removed, err := manager.Close()
		r.logRemoved(manager, len(removed), err)
		failures = append(failures, err)
	}

	r.retired = nil
	return errors.Join(failures...)
}

func (r *runtime) logRemoved(manager *anycast.Manager, count int, err error) {
	if err != nil {
		r.logger.Error("could not remove addresses", zap.String("interface", manager.Name()), zap.Error(err))
	}

	if count > 0 {
		r.logger.Info("removed addresses", zap.String("interface", manager.Name()), zap.Int("count", count))
	}
}
