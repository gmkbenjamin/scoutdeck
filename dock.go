package main

import (
	"context"
	"errors"
	"time"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msgs/std_msgs"
	"github.com/shell-company/scout/internal/control"
)

func (a *application) cancelDockNative(ctx context.Context) error {
	var errs []error
	// The search loop listens to this topic separately from both cancel services.
	if err := a.cancelDetect.WriteSync(ctx, &std_msgs.Int32{Data: 1}); err != nil {
		errs = append(errs, err)
	}
	for _, name := range []string{"/NavPathNode/nav_cancel", "/CoreNode/nav_cancel"} {
		cl, err := goroslib.NewServiceClient(goroslib.ServiceClientConf{Node: n, Name: name, Srv: &goHomeService{}})
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
	if err := a.cancelDetect.WriteSync(ctx, &std_msgs.Int32{Data: 1}); err != nil {
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
	a.control.Stop()
	if a.dock != nil {
		if err := a.dock.Cancel(ctx); err != nil {
			return err
		}
	}
	a.motionMu.Lock()
	defer a.motionMu.Unlock()
	return a.publishRaw(control.Motion{})
}
