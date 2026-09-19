package servicehost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	servicehostv1 "github.com/raulfrk/herdr-plugin-kit/runtime/servicehost/internal/servicehostv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	DefaultMaxMessageBytes  = 1 << 20
	DefaultMaxConcurrentRPC = 64
	DefaultMaxWatchers      = 32
	DefaultRPCTimeout       = 10 * time.Second
	DefaultDrainTimeout     = 60 * time.Second
	DefaultResumeTimeout    = 5 * time.Second
	DefaultShutdownTimeout  = 5 * time.Second
	MaxIdentityBytes        = 128
)

var ErrBackpressure = errors.New("servicehost: bounded capacity reached")

type MaintenanceHandler interface {
	Drain(context.Context) error
	Resume(context.Context) error
}

type ServerConfig struct {
	SocketPath       string
	ProtocolVersion  uint32
	ServiceName      string
	ServiceVersion   string
	MaxMessageBytes  int
	MaxConcurrentRPC int
	MaxWatchers      int
	RPCTimeout       time.Duration
	DrainTimeout     time.Duration
	ResumeTimeout    time.Duration
	ShutdownTimeout  time.Duration
	InitialState     HealthState
	InitialAccepting bool
	InitialDetail    string
	Maintenance      MaintenanceHandler
}

type Server struct {
	config ServerConfig
	grpc   *grpc.Server
	hub    *statusHub
	once   sync.Once

	maintenanceMu  sync.Mutex
	beforeDrain    *Status
	drainCompleted bool
}

func NewServer(config ServerConfig) (*Server, error) {
	if err := normalizeServerConfig(&config); err != nil {
		return nil, err
	}
	hub, err := newStatusHub(config.InitialState, config.InitialAccepting, config.InitialDetail, config.MaxWatchers)
	if err != nil {
		return nil, err
	}
	admission := make(chan struct{}, config.MaxConcurrentRPC)
	acquire := func() (func(), error) {
		select {
		case admission <- struct{}{}:
			return func() { <-admission }, nil
		default:
			return nil, status.Error(codes.ResourceExhausted, ErrBackpressure.Error())
		}
	}
	unary := func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		release, err := acquire()
		if err != nil {
			return nil, err
		}
		defer release()
		timeout := config.RPCTimeout
		if info.FullMethod == servicehostv1.Control_Drain_FullMethodName {
			timeout = config.DrainTimeout
		}
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return handler(bounded, request)
	}
	stream := func(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		release, err := acquire()
		if err != nil {
			return err
		}
		defer release()
		return handler(server, stream)
	}
	grpcServer := grpc.NewServer(
		grpc.Creds(unixPeerCredentials{uid: uint32(os.Getuid())}),
		grpc.MaxRecvMsgSize(config.MaxMessageBytes),
		grpc.MaxSendMsgSize(config.MaxMessageBytes),
		grpc.UnaryInterceptor(unary),
		grpc.StreamInterceptor(stream),
	)
	server := &Server{config: config, grpc: grpcServer, hub: hub}
	servicehostv1.RegisterControlServer(grpcServer, &controlServer{owner: server})
	return server, nil
}

func (s *Server) Registrar() grpc.ServiceRegistrar { return s.grpc }

func (s *Server) Status() Status { return s.hub.current() }

func (s *Server) UpdateStatus(state HealthState, accepting bool, detail string) (Status, error) {
	return s.hub.update(state, accepting, detail)
}

func (s *Server) Serve(ctx context.Context) (resultErr error) {
	if ctx == nil {
		panic("nil context")
	}
	started := false
	s.once.Do(func() { started = true })
	if !started {
		return errors.New("servicehost: server can only be served once")
	}
	listener, _, err := listenPrivateUnix(s.config.SocketPath, uint32(os.Getuid()))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, listener.Close()) }()
	served := make(chan error, 1)
	go func() { served <- s.grpc.Serve(listener) }()
	select {
	case err := <-served:
		return normalizeServeResult(err)
	case <-ctx.Done():
		_, _ = s.hub.update(HealthStopping, false, "stopping")
		stopped := make(chan struct{})
		go func() {
			s.grpc.GracefulStop()
			close(stopped)
		}()
		timer := time.NewTimer(s.config.ShutdownTimeout)
		defer timer.Stop()
		select {
		case <-stopped:
		case <-timer.C:
			s.grpc.Stop()
			<-stopped
		}
		s.hub.close()
		return nil
	}
}

func normalizeServeResult(err error) error {
	if err == nil || errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("servicehost: serve: %w", err)
}

type controlServer struct {
	servicehostv1.UnimplementedControlServer
	owner *Server
}

func (s *controlServer) Handshake(_ context.Context, request *servicehostv1.HandshakeRequest) (*servicehostv1.HandshakeResponse, error) {
	if err := s.version(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	if !validIdentity(request.GetClientName()) {
		return nil, status.Error(codes.InvalidArgument, "client name is required and must be bounded UTF-8")
	}
	current := s.owner.hub.current()
	return &servicehostv1.HandshakeResponse{
		ProtocolVersion: s.owner.config.ProtocolVersion,
		ServiceName:     s.owner.config.ServiceName,
		ServiceVersion:  s.owner.config.ServiceVersion,
		Epoch:           current.Epoch,
		Revision:        current.Revision,
	}, nil
}

func (s *controlServer) Health(_ context.Context, request *servicehostv1.HealthRequest) (*servicehostv1.HealthResponse, error) {
	if err := s.version(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	current := s.owner.hub.current()
	return healthResponse(current), nil
}

func (s *controlServer) GetStatus(_ context.Context, request *servicehostv1.GetStatusRequest) (*servicehostv1.StatusSnapshot, error) {
	if err := s.version(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	return statusSnapshot(s.owner.hub.current()), nil
}

func (s *controlServer) Drain(ctx context.Context, request *servicehostv1.DrainRequest) (*servicehostv1.StatusSnapshot, error) {
	if err := s.version(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	if s.owner.config.Maintenance == nil {
		return nil, status.Error(codes.Unimplemented, "service does not support draining")
	}
	if !s.owner.maintenanceMu.TryLock() {
		return nil, status.Error(codes.ResourceExhausted, "service maintenance is already in progress")
	}
	defer s.owner.maintenanceMu.Unlock()
	if s.owner.beforeDrain != nil {
		if !s.owner.drainCompleted {
			return nil, status.Error(codes.FailedPrecondition, "service admissions must be restored before another drain")
		}
		return statusSnapshot(s.owner.hub.current()), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, status.FromContextError(ctxErr).Err()
	}
	before := s.owner.hub.current()
	if _, err := s.owner.hub.update(HealthDraining, false, "draining"); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	drainErr := s.owner.config.Maintenance.Drain(ctx)
	if drainErr == nil {
		drainErr = ctx.Err()
	}
	if drainErr != nil {
		resumeCtx, cancel := context.WithTimeout(context.Background(), s.owner.config.ResumeTimeout)
		resumeErr := s.owner.config.Maintenance.Resume(resumeCtx)
		cancel()
		if resumeErr == nil {
			_, resumeErr = s.owner.hub.update(before.State, before.Accepting, before.Detail)
		}
		if resumeErr != nil {
			s.owner.beforeDrain = &before
			s.owner.drainCompleted = false
			_, _ = s.owner.hub.update(HealthDegraded, false, "drain failed and admissions could not be restored")
			return nil, status.Error(codes.Internal, "drain failed and admissions could not be restored")
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		return nil, status.Error(codes.Aborted, "drain failed; admissions restored")
	}
	s.owner.beforeDrain = &before
	s.owner.drainCompleted = true
	return statusSnapshot(s.owner.hub.current()), nil
}

func (s *controlServer) Resume(ctx context.Context, request *servicehostv1.ResumeRequest) (*servicehostv1.StatusSnapshot, error) {
	if err := s.version(request.GetProtocolVersion()); err != nil {
		return nil, err
	}
	if s.owner.config.Maintenance == nil {
		return nil, status.Error(codes.Unimplemented, "service does not support resuming")
	}
	if !s.owner.maintenanceMu.TryLock() {
		return nil, status.Error(codes.ResourceExhausted, "service maintenance is already in progress")
	}
	defer s.owner.maintenanceMu.Unlock()
	if s.owner.beforeDrain == nil {
		return statusSnapshot(s.owner.hub.current()), nil
	}
	if err := s.owner.config.Maintenance.Resume(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		return nil, status.Error(codes.Aborted, "admissions could not be restored")
	}
	before := *s.owner.beforeDrain
	s.owner.beforeDrain = nil
	s.owner.drainCompleted = false
	current, err := s.owner.hub.update(before.State, before.Accepting, before.Detail)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return statusSnapshot(current), nil
}

func (s *controlServer) WatchStatus(request *servicehostv1.WatchStatusRequest, stream grpc.ServerStreamingServer[servicehostv1.StatusUpdate]) error {
	if err := s.version(request.GetProtocolVersion()); err != nil {
		return err
	}
	id, updates, initial, err := s.owner.hub.subscribe(Cursor{Epoch: request.GetEpoch(), Revision: request.GetAfterRevision()})
	if err != nil {
		if errors.Is(err, ErrBackpressure) {
			return status.Error(codes.ResourceExhausted, err.Error())
		}
		return status.Error(codes.Unavailable, err.Error())
	}
	defer s.owner.hub.unsubscribe(id)
	if initial != nil {
		if err := stream.Send(statusUpdate(*initial)); err != nil {
			return err
		}
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case update, ok := <-updates:
			if !ok {
				return status.Error(codes.ResourceExhausted, "status watcher did not keep up")
			}
			if err := stream.Send(&servicehostv1.StatusUpdate{Snapshot: statusSnapshot(update)}); err != nil {
				return err
			}
		}
	}
}

func (s *controlServer) version(got uint32) error {
	if got != s.owner.config.ProtocolVersion {
		return status.Errorf(codes.FailedPrecondition, "protocol version %d is incompatible with service version %d", got, s.owner.config.ProtocolVersion)
	}
	return nil
}

func normalizeServerConfig(config *ServerConfig) error {
	if !cleanAbsolute(config.SocketPath) {
		return errors.New("servicehost: socket path must be clean and absolute")
	}
	if config.ProtocolVersion == 0 || !validIdentity(config.ServiceName) || !validIdentity(config.ServiceVersion) {
		return errors.New("servicehost: protocol, service name, and service version are required")
	}
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if config.MaxConcurrentRPC == 0 {
		config.MaxConcurrentRPC = DefaultMaxConcurrentRPC
	}
	if config.MaxWatchers == 0 {
		config.MaxWatchers = DefaultMaxWatchers
	}
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = DefaultShutdownTimeout
	}
	if config.RPCTimeout == 0 {
		config.RPCTimeout = DefaultRPCTimeout
	}
	if config.DrainTimeout == 0 {
		config.DrainTimeout = DefaultDrainTimeout
	}
	if config.ResumeTimeout == 0 {
		config.ResumeTimeout = DefaultResumeTimeout
	}
	if config.InitialState == 0 {
		config.InitialState = HealthStarting
	}
	if config.MaxMessageBytes < 1 || config.MaxConcurrentRPC < 1 || config.MaxWatchers < 1 || config.RPCTimeout < 1 || config.DrainTimeout < 1 || config.ResumeTimeout < 1 || config.ShutdownTimeout < 1 {
		return errors.New("servicehost: bounds and shutdown timeout must be positive")
	}
	return validateStatus(config.InitialState, config.InitialDetail)
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= MaxIdentityBytes && utf8.ValidString(value)
}

func healthResponse(current Status) *servicehostv1.HealthResponse {
	return &servicehostv1.HealthResponse{State: protobufHealth(current.State), Accepting: current.Accepting, Detail: current.Detail, Epoch: current.Epoch, Revision: current.Revision}
}

func statusSnapshot(current Status) *servicehostv1.StatusSnapshot {
	return &servicehostv1.StatusSnapshot{Epoch: current.Epoch, Revision: current.Revision, State: protobufHealth(current.State), Accepting: current.Accepting, Detail: current.Detail}
}

func statusUpdate(update StatusUpdate) *servicehostv1.StatusUpdate {
	return &servicehostv1.StatusUpdate{Snapshot: statusSnapshot(update.Status), ResetRequired: update.Reset}
}

func protobufHealth(state HealthState) servicehostv1.HealthState {
	return servicehostv1.HealthState(state)
}

func publicHealth(state servicehostv1.HealthState) HealthState {
	return HealthState(state)
}

var _ io.Closer = (*privateListener)(nil)
