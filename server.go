package ch

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/go-faster/errors"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/go-faster/ch/internal/compress"
	"github.com/go-faster/ch/proto"
)

// Server is basic ClickHouse server.
type Server struct {
	lg      *zap.Logger
	tz      *time.Location
	workers int
	conn    atomic.Uint64
}

// ServerOptions wraps possible Server configuration.
type ServerOptions struct {
	Logger   *zap.Logger
	Timezone *time.Location
	Workers  int
}

// NewServer returns new ClickHouse Server.
func NewServer(opt ServerOptions) *Server {
	if opt.Logger == nil {
		opt.Logger = zap.NewNop()
	}
	if opt.Timezone == nil {
		opt.Timezone = time.UTC
	}
	if opt.Workers == 0 {
		opt.Workers = 100
	}
	return &Server{
		lg:      opt.Logger,
		tz:      opt.Timezone,
		workers: opt.Workers,
	}
}

// ServerConn wraps Server connection.
type ServerConn struct {
	lg     *zap.Logger
	tz     *time.Location
	conn   net.Conn
	buf    *proto.Buffer
	reader *proto.Reader
	client proto.ClientHello
	info   proto.ServerHello

	// compressor performs block compression,
	// see encodeBlock.
	compressor *compress.Writer

	settings []Setting
}

func (c *ServerConn) packet() (proto.ClientCode, error) {
	n, err := c.reader.UVarInt()
	if err != nil {
		return 0, errors.Wrap(err, "uvarint")
	}

	code := proto.ClientCode(n)
	if ce := c.lg.Check(zap.DebugLevel, "Packet"); ce != nil {
		ce.Write(
			zap.Uint64("packet_code_raw", n),
			zap.Stringer("packet_code", code),
		)
	}
	if !code.IsAClientCode() {
		return 0, errors.Errorf("bad client packet type %d", n)
	}

	return code, nil
}

func (c *ServerConn) handshake() error {
	p, err := c.packet()
	if err != nil {
		return errors.Wrap(err, "packet")
	}
	if p != proto.ClientCodeHello {
		return errors.Errorf("unexpected packet %q", p)
	}
	if err := c.client.Decode(c.reader); err != nil {
		return errors.Wrap(err, "decode hello")
	}
	c.info.EncodeAware(c.buf, c.client.ProtocolVersion)
	if err := c.flush(); err != nil {
		return errors.Wrap(err, "flush")
	}

	_ = c.compressor // hack
	_ = c.settings   // hack

	return nil
}

func (c *ServerConn) flush() error {
	n, err := c.conn.Write(c.buf.Buf)
	if err != nil {
		return errors.Wrap(err, "write")
	}
	if n != len(c.buf.Buf) {
		return errors.Wrap(io.ErrShortWrite, "wrote less than expected")
	}
	if ce := c.lg.Check(zap.DebugLevel, "Flush"); ce != nil {
		ce.Write(zap.Int("bytes", n))
	}
	c.buf.Reset()
	return nil
}

func (c *ServerConn) handlePacket(p proto.ClientCode) error {
	switch p {
	case proto.ClientCodePing:
		return c.handlePing()
	default:
		return errors.Errorf("%q not implemented", p)
	}
}

func (c *ServerConn) handlePing() error {
	proto.ServerCodePong.Encode(c.buf)
	return c.flush()
}

// Handle connection.
func (c *ServerConn) Handle() error {
	if err := c.handshake(); err != nil {
		return errors.Wrap(err, "handshake")
	}
	for {
		p, err := c.packet()
		if err != nil {
			return errors.Wrap(err, "packet")
		}
		c.lg.Debug("Packet", zap.String("packet", p.String()))
		if err := c.handlePacket(p); err != nil {
			return errors.Wrapf(err, "handle %q", p)
		}
	}
}

func (s *Server) handle(conn net.Conn) error {
	lg := s.lg.With(
		zap.Uint64("conn", s.conn.Inc()),
	)
	lg.Info("Connected",
		zap.String("addr", conn.RemoteAddr().String()),
	)
	sConn := &ServerConn{
		lg:     lg,
		conn:   conn,
		buf:    new(proto.Buffer),
		reader: proto.NewReader(conn),
		client: proto.ClientHello{},
		info: proto.ServerHello{
			Name: "CH",
		},
		tz:         time.UTC,
		compressor: compress.NewWriter(),
	}
	return sConn.Handle()
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := ln.Accept()
		if err != nil {
			return errors.Wrap(err, "accept")
		}
		if err := s.handle(c); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				continue
			}
			s.lg.Error("Handle", zap.Error(err))
		}
	}
}

// Serve connections on net.Listener.
func (s *Server) Serve(ln net.Listener) error {
	g, ctx := errgroup.WithContext(context.Background())
	for i := 0; i < s.workers; i++ {
		g.Go(func() error {
			return s.serve(ctx, ln)
		})
	}
	return g.Wait()
}