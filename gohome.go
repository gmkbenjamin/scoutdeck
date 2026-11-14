package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aler9/goroslib"
	"github.com/aler9/goroslib/pkg/msg"
)

type goHomeRequest struct {
}

type goHomeResponse struct {
}

type goHomeService struct {
	msg.Package `ros:"/nav_low_bat"`
	Request     goHomeRequest
	Response    goHomeResponse
}

// Start the firmware's QR search, alignment and reverse docking sequence.
func scoutGoHome(ctx context.Context) error {
	node := rosNode()
	if node == nil {
		return fmt.Errorf("ros node is not initialized")
	}

	cl, err := goroslib.NewServiceClient(goroslib.ServiceClientConf{
		Node: node,
		Name: "/nav_low_bat",
		Srv: &goHomeService{
			Request: goHomeRequest{},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create go-home service client: %w", err)
	}
	defer cl.Close()

	req := goHomeRequest{}
	res := goHomeResponse{}

	err = cl.CallContext(ctx, &req, &res)
	if err != nil {
		return fmt.Errorf("go-home service call failed: %w", err)
	}
	log.Println("Command received: Scout returning home in 5 seconds")
	return nil
}
