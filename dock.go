package main

import (
	"context"
	"errors"
	"time"

	"github.com/aler9/goroslib"
	"github.com/gmkbenjamin/scoutdeck/internal/control"
)

func (a *application) cancelDockNative(ctx context.Context) error {
	var errs []error
	// The search loop listens to this topic separately from both cancel services.
	if err := a.writeCancelDetect(ctx); err != nil {
		errs = append(errs, err)
	}
	node := rosNode()
	if node == nil {
		errs = append(errs, errors.New("not connected to the robot"))
	} else {
		for _, name := range []string{"/NavPathNode/nav_cancel", "/CoreNode/nav_cancel"} {
			cl, err := goroslib.NewServiceClient(goroslib.ServiceClientConf{Node: node, Name: name, Srv: &goHomeService{}})
			if err == nil {
				call, cancel := context.WithTimeout(ctx, 3*time.Second)
				err = cl.CallContext(call, &goHomeRequest{}, &goHomeResponse{})
				cancel()
				_ = cl.Close()
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := a.writeCancelDetect(ctx); err != nil {
		errs = append(errs, err)
	}
	a.motionMu.Lock()
	err := a.publishRaw(control.Motion{})
	a.motionMu.Unlock()
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (a *application) stopAll(ctx context.Context) error {
	a.humanAt.Store(time.Now().UnixNano())
	a.act.stop()
	a.control.Stop()
	if d := a.dockManager(); d != nil {
		if err := d.Cancel(ctx); err != nil {
			return err
		}
	}
	a.motionMu.Lock()
	defer a.motionMu.Unlock()
	return a.publishRaw(control.Motion{})
}
