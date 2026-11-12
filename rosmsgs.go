package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aler9/goroslib/pkg/msg"
)

// Firmware (roller_eye) and dynamic_reconfigure types used by the newer
// features. Field order and types match `rosmsg show` / `rossrv show` on the
// robot; goroslib derives the checksums from them.

// detectMsg is roller_eye/detect, published on /CoreNode/obj (people, cats,
// dogs from the on-robot TFLite model) and /CoreNode/motion.
type detectMsg struct {
	msg.Package `ros:"roller_eye"`
	Seq         uint32
	Score       float32
	Index       int32
	Name        string
	Width       int32
	Height      int32
	Top         float32
	Left        float32
	Bottom      float32
	Right       float32
	Stamp       uint64
}

// callQuick makes one firmware service call with a short timeout.
func callQuick(ctx context.Context, name string, srv, req, res any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := callService(ctx, name, srv, req, res); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// --- On-robot recordings (RecorderAgentNode) ---

type recordEntry struct {
	msg.Definitions `ros:"int8 RECORD_TYPE_ALL=0,int8 RECORD_TYPE_SNAPSHOT=1,int8 RECORD_TYPE_RECORD=2,int8 RECORD_TYPE_THUMB=3,int8 RECORD_TYPE_SCHED_RECORD=4"`
	Id              string
	Name            string
	Dur             uint32
	Type            int8
	Create          time.Time
	Size            uint32
}

type recordGetFilesReq struct {
	Type  int8
	Id    string
	Start int32
	Size  int32
}
type recordGetFilesRes struct {
	Files []recordEntry
}
type recordGetFilesSrv struct {
	msg.Package `ros:"roller_eye"`
	Request     recordGetFilesReq
	Response    recordGetFilesRes
}

type recordIDReq struct{ Id string }
type recordPathRes struct{ Path string }
type recordGetPathSrv struct {
	msg.Package `ros:"roller_eye"`
	Request     recordIDReq
	Response    recordPathRes
}
type recordDeleteRes struct{ Status int8 }
type recordDeleteSrv struct {
	msg.Package `ros:"roller_eye"`
	Request     recordIDReq
	Response    recordDeleteRes
}

// --- Camera: dynamic_reconfigure on /ParamNode/video ---

type drBool struct {
	Name  string
	Value bool
}
type drInt struct {
	Name  string
	Value int32
}
type drStr struct {
	Name  string
	Value string
}
type drDouble struct {
	Name  string
	Value float64
}
type drGroup struct {
	Name   string
	State  bool
	Id     int32
	Parent int32
}
type drConfig struct {
	Bools   []drBool
	Ints    []drInt
	Strs    []drStr
	Doubles []drDouble
	Groups  []drGroup
}
type reconfigureReq struct{ Config drConfig }
type reconfigureRes struct{ Config drConfig }
type reconfigureSrv struct {
	msg.Package `ros:"dynamic_reconfigure"`
	Request     reconfigureReq
	Response    reconfigureRes
}

// --- Night mode report ---

type nightGetReq struct{}
type nightGetRes struct {
	IsNight    int8  `rosname:"isNight"`
	Brightness int32 `rosname:"brightness"`
}
type nightGetSrv struct {
	msg.Package `ros:"roller_eye"`
	Request     nightGetReq
	Response    nightGetRes
}
