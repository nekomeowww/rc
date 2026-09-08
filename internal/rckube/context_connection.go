package rckube

import (
	"context"
	"net"
)

// contextConnection binds cancellation to the entire connection lifetime,
// including response and stream reads after DialContext has completed.
type contextConnection struct {
	net.Conn
	ctx  context.Context
	stop func() bool
}

func bindConnectionContext(ctx context.Context, connection net.Conn) net.Conn {
	return &contextConnection{
		Conn: connection, ctx: ctx,
		stop: context.AfterFunc(ctx, func() { _ = connection.Close() }),
	}
}

func (connection *contextConnection) Read(data []byte) (int, error) {
	n, err := connection.Conn.Read(data)
	if err != nil && connection.ctx.Err() != nil {
		return n, connection.ctx.Err()
	}
	return n, err
}

func (connection *contextConnection) Write(data []byte) (int, error) {
	n, err := connection.Conn.Write(data)
	if err != nil && connection.ctx.Err() != nil {
		return n, connection.ctx.Err()
	}
	return n, err
}

func (connection *contextConnection) Close() error {
	connection.stop()
	return connection.Conn.Close()
}
