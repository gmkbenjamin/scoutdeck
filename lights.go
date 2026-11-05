package main

import (
	"context"
	"fmt"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msg"
)

type adjustLightRequest struct {
	Cmd int32 `ros:"cmd int32"`
}

type adjustLightResponse struct {
}

type adjustLightService struct {
	msg.Package `ros:"/CoreNode/adjust_light"`
	Request     adjustLightRequest
	Response    adjustLightResponse
}

// call the /CoreNode/adjust_light service with a value of 1 to turn on the light
func turnOnLight(ctx context.Context, lightValue int32) error {
	if n == nil {
		return fmt.Errorf("ros node is not initialized")
	}

	cl, err := goroslib.NewServiceClient(goroslib.ServiceClientConf{
		Node: n,
		Name: "/CoreNode/adjust_light",
		Srv: &adjustLightService{
			Request: adjustLightRequest{Cmd: lightValue},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create light service client: %w", err)
	}
	defer cl.Close()

	req := adjustLightRequest{Cmd: lightValue}
	res := adjustLightResponse{}

	if err := cl.CallContext(ctx, &req, &res); err != nil {
		return fmt.Errorf("light service call failed: %w", err)
	}
	return nil
}
