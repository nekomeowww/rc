/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package envdconnectrpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	procpb "github.com/nekomeowww/rc/apis/sdk/go/envd/process"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rckube"
)

type processHandler struct {
	server *Server
}

func (h *processHandler) List(_ context.Context, req *connect.Request[procpb.ListRequest]) (*connect.Response[procpb.ListResponse], error) {
	if err := h.server.validateRPCUser(req.Header()); err != nil {
		return nil, err
	}
	infos := h.server.supervisor.ListDataPlaneProcesses()
	processes := make([]*procpb.ProcessInfo, 0, len(infos))
	for _, info := range infos {
		var tag *string
		if info.Tag != "" {
			t := info.Tag
			tag = &t
		}
		var cwd *string
		if info.Config.Cwd != "" {
			c := info.Config.Cwd
			cwd = &c
		}
		processes = append(processes, &procpb.ProcessInfo{
			Pid: info.PID,
			Tag: tag,
			Config: &procpb.ProcessConfig{
				Cmd:  info.Config.Cmd,
				Args: info.Config.Args,
				Envs: info.Config.Envs,
				Cwd:  cwd,
			},
		})
	}
	return connect.NewResponse(&procpb.ListResponse{Processes: processes}), nil
}

func (h *processHandler) Start(ctx context.Context, req *connect.Request[procpb.StartRequest], stream *connect.ServerStream[procpb.StartResponse]) error {
	if err := h.server.validateRPCUser(req.Header()); err != nil {
		return err
	}
	procCfg := req.Msg.GetProcess()
	if procCfg == nil || procCfg.GetCmd() == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("process.cmd is required"))
	}

	cwd := h.server.homeDir
	if procCfg.Cwd != nil {
		cwd = h.server.resolvePath(procCfg.GetCwd())
	}

	stdinEnabled := true
	if req.Msg.Stdin != nil {
		stdinEnabled = req.Msg.GetStdin()
	}

	cfg := rckube.DataPlaneProcessConfig{
		Cmd:   procCfg.GetCmd(),
		Args:  procCfg.GetArgs(),
		Envs:  procCfg.GetEnvs(),
		Cwd:   cwd,
		Tag:   req.Msg.GetTag(),
		Stdin: stdinEnabled,
	}
	if ptyCfg := req.Msg.GetPty(); ptyCfg != nil {
		cfg.PTY = true
		if sz := ptyCfg.GetSize(); sz != nil {
			cfg.Cols = uint16(sz.GetCols())
			cfg.Rows = uint16(sz.GetRows())
		}
	}

	info, sub, err := h.server.supervisor.StartDataPlaneProcess(ctx, cfg)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	defer sub.Close()

	return streamProcessEvents(ctx, info, sub, func(ev *procpb.ProcessEvent) error {
		return stream.Send(&procpb.StartResponse{Event: ev})
	})
}

func (h *processHandler) Connect(ctx context.Context, req *connect.Request[procpb.ConnectRequest], stream *connect.ServerStream[procpb.ConnectResponse]) error {
	if err := h.server.validateRPCUser(req.Header()); err != nil {
		return err
	}
	pid, tag, err := parseSelector(req.Msg.GetProcess())
	if err != nil {
		return err
	}
	info, sub, err := h.server.supervisor.ConnectDataPlaneProcess(pid, tag)
	if err != nil {
		return mapProcessError(err)
	}
	defer sub.Close()

	return streamProcessEvents(ctx, info, sub, func(ev *procpb.ProcessEvent) error {
		return stream.Send(&procpb.ConnectResponse{Event: ev})
	})
}

func (h *processHandler) Update(_ context.Context, req *connect.Request[procpb.UpdateRequest]) (*connect.Response[procpb.UpdateResponse], error) {
	if err := h.server.validateRPCUser(req.Header()); err != nil {
		return nil, err
	}
	pid, tag, err := parseSelector(req.Msg.GetProcess())
	if err != nil {
		return nil, err
	}
	if ptyCfg := req.Msg.GetPty(); ptyCfg != nil && ptyCfg.GetSize() != nil {
		sz := ptyCfg.GetSize()
		if err := h.server.supervisor.ResizeDataPlanePTY(pid, tag, uint16(sz.GetCols()), uint16(sz.GetRows())); err != nil {
			return nil, mapProcessError(err)
		}
	}
	return connect.NewResponse(&procpb.UpdateResponse{}), nil
}

func (h *processHandler) SendInput(_ context.Context, req *connect.Request[procpb.SendInputRequest]) (*connect.Response[procpb.SendInputResponse], error) {
	if err := h.server.validateRPCUser(req.Header()); err != nil {
		return nil, err
	}
	pid, tag, err := parseSelector(req.Msg.GetProcess())
	if err != nil {
		return nil, err
	}
	data := extractInputBytes(req.Msg.GetInput())
	if err := h.server.supervisor.SendDataPlaneInput(pid, tag, data); err != nil {
		return nil, mapProcessError(err)
	}
	return connect.NewResponse(&procpb.SendInputResponse{}), nil
}

func (h *processHandler) StreamInput(_ context.Context, stream *connect.ClientStream[procpb.StreamInputRequest]) (*connect.Response[procpb.StreamInputResponse], error) {
	if err := h.server.validateRPCUser(stream.RequestHeader()); err != nil {
		return nil, err
	}
	var pid uint32
	var tag string
	selected := false

	for stream.Receive() {
		msg := stream.Msg()
		switch ev := msg.Event.(type) {
		case *procpb.StreamInputRequest_Start:
			var err error
			pid, tag, err = parseSelector(ev.Start.GetProcess())
			if err != nil {
				return nil, err
			}
			selected = true
		case *procpb.StreamInputRequest_Data:
			if !selected {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("start event must precede data events"))
			}
			data := extractInputBytes(ev.Data.GetInput())
			if err := h.server.supervisor.SendDataPlaneInput(pid, tag, data); err != nil {
				return nil, mapProcessError(err)
			}
		case *procpb.StreamInputRequest_Keepalive:
			// No-op keepalive.
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&procpb.StreamInputResponse{}), nil
}

func (h *processHandler) CloseStdin(_ context.Context, req *connect.Request[procpb.CloseStdinRequest]) (*connect.Response[procpb.CloseStdinResponse], error) {
	if err := h.server.validateRPCUser(req.Header()); err != nil {
		return nil, err
	}
	pid, tag, err := parseSelector(req.Msg.GetProcess())
	if err != nil {
		return nil, err
	}
	if err := h.server.supervisor.CloseDataPlaneStdin(pid, tag); err != nil {
		return nil, mapProcessError(err)
	}
	return connect.NewResponse(&procpb.CloseStdinResponse{}), nil
}

func (h *processHandler) SendSignal(_ context.Context, req *connect.Request[procpb.SendSignalRequest]) (*connect.Response[procpb.SendSignalResponse], error) {
	if err := h.server.validateRPCUser(req.Header()); err != nil {
		return nil, err
	}
	pid, tag, err := parseSelector(req.Msg.GetProcess())
	if err != nil {
		return nil, err
	}
	force := req.Msg.GetSignal() == procpb.Signal_SIGNAL_SIGKILL
	if err := h.server.supervisor.SendDataPlaneSignal(pid, tag, force); err != nil {
		return nil, mapProcessError(err)
	}
	return connect.NewResponse(&procpb.SendSignalResponse{}), nil
}

func streamProcessEvents(ctx context.Context, info rckube.DataPlaneProcessInfo, sub *rckube.DataPlaneSubscription, send func(*procpb.ProcessEvent) error) error {
	if err := send(&procpb.ProcessEvent{
		Event: &procpb.ProcessEvent_Start{
			Start: &procpb.ProcessEvent_StartEvent{Pid: info.PID},
		},
	}); err != nil {
		return err
	}

	for _, chunk := range sub.Replay() {
		if err := send(chunkToEvent(chunk)); err != nil {
			return err
		}
	}

	if sub.Chunks() == nil {
		return send(outcomeToEndEvent(sub.Outcome()))
	}

	ticker := time.NewTicker(KeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := send(&procpb.ProcessEvent{
				Event: &procpb.ProcessEvent_Keepalive{Keepalive: &procpb.ProcessEvent_KeepAlive{}},
			}); err != nil {
				return err
			}
		case chunk := <-sub.Chunks():
			if err := send(chunkToEvent(chunk)); err != nil {
				return err
			}
		case <-sub.Exit():
			for {
				select {
				case chunk := <-sub.Chunks():
					if err := send(chunkToEvent(chunk)); err != nil {
						return err
					}
				default:
					return send(outcomeToEndEvent(sub.Outcome()))
				}
			}
		}
	}
}

func chunkToEvent(chunk rckube.DataPlaneChunk) *procpb.ProcessEvent {
	var dataEvent *procpb.ProcessEvent_DataEvent
	switch chunk.Kind {
	case rckube.DataPlaneStreamStderr:
		dataEvent = &procpb.ProcessEvent_DataEvent{
			Output: &procpb.ProcessEvent_DataEvent_Stderr{Stderr: chunk.Data},
		}
	case rckube.DataPlaneStreamPTY:
		dataEvent = &procpb.ProcessEvent_DataEvent{
			Output: &procpb.ProcessEvent_DataEvent_Pty{Pty: chunk.Data},
		}
	default:
		dataEvent = &procpb.ProcessEvent_DataEvent{
			Output: &procpb.ProcessEvent_DataEvent_Stdout{Stdout: chunk.Data},
		}
	}
	return &procpb.ProcessEvent{
		Event: &procpb.ProcessEvent_Data{Data: dataEvent},
	}
}

func outcomeToEndEvent(outcome rckube.DataPlaneProcessInfo) *procpb.ProcessEvent {
	var errMsg *string
	if outcome.Error != "" {
		e := outcome.Error
		errMsg = &e
	}
	return &procpb.ProcessEvent{
		Event: &procpb.ProcessEvent_End{
			End: &procpb.ProcessEvent_EndEvent{
				ExitCode: outcome.ExitCode,
				Exited:   outcome.Exited,
				Status:   outcome.Status,
				Error:    errMsg,
			},
		},
	}
}

func parseSelector(sel *procpb.ProcessSelector) (uint32, string, error) {
	if sel == nil {
		return 0, "", connect.NewError(connect.CodeInvalidArgument, errors.New("process selector is required"))
	}
	switch s := sel.Selector.(type) {
	case *procpb.ProcessSelector_Pid:
		if s.Pid == 0 {
			return 0, "", connect.NewError(connect.CodeInvalidArgument, errors.New("pid must be non-zero"))
		}
		return s.Pid, "", nil
	case *procpb.ProcessSelector_Tag:
		if s.Tag == "" {
			return 0, "", connect.NewError(connect.CodeInvalidArgument, errors.New("tag must be non-empty"))
		}
		return 0, s.Tag, nil
	default:
		return 0, "", connect.NewError(connect.CodeInvalidArgument, errors.New("process selector must specify pid or tag"))
	}
}

func extractInputBytes(input *procpb.ProcessInput) []byte {
	if input == nil {
		return nil
	}
	switch v := input.Input.(type) {
	case *procpb.ProcessInput_Stdin:
		return v.Stdin
	case *procpb.ProcessInput_Pty:
		return v.Pty
	default:
		return nil
	}
}

func mapProcessError(err error) error {
	if errors.Is(err, processruntime.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("process not found: %w", err))
	}
	return connect.NewError(connect.CodeFailedPrecondition, err)
}
