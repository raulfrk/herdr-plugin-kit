package servicehost

import (
	"context"
	"errors"
	"net"
	"os"
	"time"
	"unicode/utf8"

	servicehostv1 "github.com/raulfrk/herdr-plugin-kit/runtime/servicehost/internal/servicehostv1"
	"google.golang.org/grpc"
)

type ClientConfig struct {
	SocketPath      string
	ProtocolVersion uint32
	ClientName      string
	MaxMessageBytes int
	DialTimeout     time.Duration
	RPCTimeout      time.Duration
	DrainTimeout    time.Duration
}

type Handshake struct {
	ProtocolVersion uint32
	ServiceName     string
	ServiceVersion  string
	Cursor          Cursor
}

type Client struct {
	config     ClientConfig
	connection *grpc.ClientConn
	control    servicehostv1.ControlClient
	handshake  Handshake
}

func Dial(ctx context.Context, config ClientConfig) (*Client, error) {
	if ctx == nil {
		panic("nil context")
	}
	if !cleanAbsolute(config.SocketPath) || config.ProtocolVersion == 0 || !validIdentity(config.ClientName) {
		return nil, errors.New("servicehost: valid socket, protocol version, and client name are required")
	}
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = 2 * time.Second
	}
	if config.RPCTimeout == 0 {
		config.RPCTimeout = DefaultRPCTimeout
	}
	if config.DrainTimeout == 0 {
		config.DrainTimeout = DefaultDrainTimeout
	}
	if config.MaxMessageBytes < 1 || config.DialTimeout < 1 || config.RPCTimeout < 1 || config.DrainTimeout < 1 || !utf8.ValidString(config.ClientName) {
		return nil, errors.New("servicehost: invalid client bounds")
	}
	dialCtx, cancel := context.WithTimeout(ctx, config.DialTimeout)
	defer cancel()
	connection, err := grpc.DialContext(dialCtx, "unix://"+config.SocketPath,
		grpc.WithTransportCredentials(unixPeerCredentials{uid: uint32(os.Getuid())}),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", config.SocketPath)
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(config.MaxMessageBytes),
			grpc.MaxCallSendMsgSize(config.MaxMessageBytes),
		),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, request, response any, connection *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
			timeout := config.RPCTimeout
			if method == servicehostv1.Control_Drain_FullMethodName {
				timeout = config.DrainTimeout
			}
			bounded, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return invoke(bounded, method, request, response, connection, options...)
		}),
	)
	if err != nil {
		return nil, err
	}
	client := &Client{config: config, connection: connection, control: servicehostv1.NewControlClient(connection)}
	response, err := client.control.Handshake(dialCtx, &servicehostv1.HandshakeRequest{ProtocolVersion: config.ProtocolVersion, ClientName: config.ClientName})
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	client.handshake = Handshake{
		ProtocolVersion: response.GetProtocolVersion(), ServiceName: response.GetServiceName(), ServiceVersion: response.GetServiceVersion(),
		Cursor: Cursor{Epoch: response.GetEpoch(), Revision: response.GetRevision()},
	}
	return client, nil
}

func (c *Client) Close() error { return c.connection.Close() }

func (c *Client) Conn() grpc.ClientConnInterface { return c.connection }

func (c *Client) Handshake() Handshake { return c.handshake }

func (c *Client) Health(ctx context.Context) (Status, error) {
	response, err := c.control.Health(ctx, &servicehostv1.HealthRequest{ProtocolVersion: c.config.ProtocolVersion})
	if err != nil {
		return Status{}, err
	}
	return Status{Epoch: response.GetEpoch(), Revision: response.GetRevision(), State: publicHealth(response.GetState()), Accepting: response.GetAccepting(), Detail: response.GetDetail()}, nil
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	response, err := c.control.GetStatus(ctx, &servicehostv1.GetStatusRequest{ProtocolVersion: c.config.ProtocolVersion})
	if err != nil {
		return Status{}, err
	}
	return publicStatus(response), nil
}

func (c *Client) Drain(ctx context.Context) (Status, error) {
	response, err := c.control.Drain(ctx, &servicehostv1.DrainRequest{ProtocolVersion: c.config.ProtocolVersion})
	if err != nil {
		return Status{}, err
	}
	return publicStatus(response), nil
}

func (c *Client) Resume(ctx context.Context) (Status, error) {
	response, err := c.control.Resume(ctx, &servicehostv1.ResumeRequest{ProtocolVersion: c.config.ProtocolVersion})
	if err != nil {
		return Status{}, err
	}
	return publicStatus(response), nil
}

type Watcher struct {
	stream servicehostv1.Control_WatchStatusClient
}

func (c *Client) Watch(ctx context.Context, cursor Cursor) (*Watcher, error) {
	stream, err := c.control.WatchStatus(ctx, &servicehostv1.WatchStatusRequest{ProtocolVersion: c.config.ProtocolVersion, Epoch: cursor.Epoch, AfterRevision: cursor.Revision})
	if err != nil {
		return nil, err
	}
	return &Watcher{stream: stream}, nil
}

func (w *Watcher) Recv() (StatusUpdate, error) {
	update, err := w.stream.Recv()
	if err != nil {
		return StatusUpdate{}, err
	}
	return StatusUpdate{Status: publicStatus(update.GetSnapshot()), Reset: update.GetResetRequired()}, nil
}

func publicStatus(snapshot *servicehostv1.StatusSnapshot) Status {
	if snapshot == nil {
		return Status{}
	}
	return Status{Epoch: snapshot.GetEpoch(), Revision: snapshot.GetRevision(), State: publicHealth(snapshot.GetState()), Accepting: snapshot.GetAccepting(), Detail: snapshot.GetDetail()}
}
